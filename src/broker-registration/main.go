package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cloudfoundry/go-cfclient/v3/resource"
	"github.com/cloudfoundry/juicefs-release/src/broker-registration/internal/cloudfoundry"
)

type config struct {
	APIURL, CACert, BrokerName, BrokerURL string
	BrokerUsername, BrokerPassword        string
	CFUsername, CFPassword                string
	BrokerPort                            int
	SkipSSLValidation                     bool
	SkipBrokerSSLValidation               bool
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "register" && os.Args[1] != "deregister") {
		fatal(errors.New("usage: broker-registration register|deregister"))
	}
	path := os.Getenv("BROKER_REGISTRATION_CONFIG")
	if path == "" {
		path = "/var/vcap/jobs/register-broker/config/config.json"
		if os.Args[1] == "deregister" {
			path = "/var/vcap/jobs/deregister-broker/config/config.json"
		}
	}
	if err := run(os.Args[1], path); err != nil {
		fatal(err)
	}
}

func run(mode, path string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config
	if err = json.Unmarshal(contents, &cfg); err != nil {
		return err
	}
	cfg, err = prepareConfig(cfg)
	if err != nil {
		return err
	}
	api, err := cloudfoundry.New(cfg.APIURL, cfg.CFUsername, cfg.CFPassword, cfg.CACert, cfg.SkipSSLValidation)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return runRegistration(ctx, mode, cfg, api)
}

func prepareConfig(cfg config) (config, error) {
	cfg.CFUsername = envOr("CF_USERNAME", cfg.CFUsername)
	cfg.CFPassword = envOr("CF_PASSWORD", cfg.CFPassword)
	cfg.BrokerUsername = envOr("BROKER_USERNAME", cfg.BrokerUsername)
	cfg.BrokerPassword = envOr("BROKER_PASSWORD", cfg.BrokerPassword)
	cfg.APIURL = normalizeAPIURL(envOr("CF_API_URL", cfg.APIURL))
	cfg.CACert = envOr("CF_CA_CERT", cfg.CACert)
	if strings.EqualFold(os.Getenv("BROKER_TLS_SKIP"), "true") {
		cfg.SkipBrokerSSLValidation = true
	}
	if cfg.APIURL == "https://" || cfg.CFUsername == "" || cfg.CFPassword == "" || cfg.BrokerName == "" || cfg.BrokerUsername == "" || cfg.BrokerPassword == "" {
		return cfg, errors.New("Cloud Controller and broker link credentials are required")
	}
	if cfg.BrokerURL == "" {
		if cfg.BrokerPort == 0 {
			return cfg, errors.New("broker URL or linked broker port is required")
		}
		cfg.BrokerURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.BrokerPort)
	}
	if strings.HasPrefix(cfg.BrokerURL, "https://") && cfg.SkipBrokerSSLValidation {
		return cfg, errors.New("broker TLS verification is performed by Cloud Controller; provide a trusted broker certificate")
	}
	return cfg, nil
}

func runRegistration(ctx context.Context, mode string, cfg config, api cloudfoundry.API) error {
	if mode != "register" && mode != "deregister" {
		return errors.New("invalid registration mode")
	}
	brokers, err := api.Brokers(ctx, cfg.BrokerName)
	if err != nil {
		return fmt.Errorf("discover service broker: %w", err)
	}
	if len(brokers) > 1 {
		return errors.New("multiple service brokers match the configured name")
	}
	if mode == "deregister" {
		if len(brokers) == 0 {
			return nil
		}
		return deregister(ctx, api, brokers[0].GUID)
	}
	if len(brokers) == 0 {
		err = api.CreateBroker(ctx, resource.NewServiceBrokerCreate(cfg.BrokerName, cfg.BrokerURL, cfg.BrokerUsername, cfg.BrokerPassword))
	} else {
		err = api.UpdateBroker(ctx, brokers[0].GUID, resource.NewServiceBrokerUpdate().WithURL(cfg.BrokerURL).WithCredentials(cfg.BrokerUsername, cfg.BrokerPassword))
	}
	if err != nil {
		return fmt.Errorf("register service broker: %w", err)
	}
	// Broker creation/update is asynchronous. The adapter waits for the job
	// before rediscovering the GUID and the catalog imported by that job.
	brokers, err = api.Brokers(ctx, cfg.BrokerName)
	if err != nil {
		return fmt.Errorf("discover registered broker: %w", err)
	}
	if len(brokers) != 1 {
		return errors.New("service broker not found after registration")
	}
	offerings, err := api.Offerings(ctx, brokers[0].GUID)
	if err != nil {
		return fmt.Errorf("discover JuiceFS offering: %w", err)
	}
	if len(offerings) != 1 {
		return errors.New("expected one JuiceFS service offering after registration")
	}
	plans, err := api.Plans(ctx, offerings[0].GUID)
	if err != nil {
		return fmt.Errorf("discover service plans: %w", err)
	}
	if len(plans) == 0 {
		return errors.New("no JuiceFS service plans found after registration")
	}
	for _, plan := range plans {
		if err = api.EnablePlan(ctx, plan.GUID); err != nil {
			return fmt.Errorf("enable service plan %s: %w", plan.GUID, err)
		}
	}
	return nil
}

func deregister(ctx context.Context, api cloudfoundry.API, brokerGUID string) error {
	offerings, err := api.Offerings(ctx, brokerGUID)
	if err != nil {
		return fmt.Errorf("discover offerings to remove: %w", err)
	}
	for _, offering := range offerings {
		plans, err := api.Plans(ctx, offering.GUID)
		if err != nil {
			return fmt.Errorf("discover plans to remove: %w", err)
		}
		for _, plan := range plans {
			// CAPI refuses to delete an in-use plan. Never purge service-instance
			// records or bypass their backend deprovision lifecycle.
			if err = api.DeletePlan(ctx, plan.GUID); err != nil {
				return fmt.Errorf("delete service plan %s: %w", plan.GUID, err)
			}
		}
		if err = api.DeleteOffering(ctx, offering.GUID); err != nil {
			return fmt.Errorf("delete service offering %s: %w", offering.GUID, err)
		}
	}
	if err = api.DeleteBroker(ctx, brokerGUID); err != nil {
		return fmt.Errorf("delete service broker: %w", err)
	}
	return nil
}

func normalizeAPIURL(apiURL string) string {
	if strings.HasPrefix(apiURL, "http://") || strings.HasPrefix(apiURL, "https://") {
		return apiURL
	}
	return "https://" + apiURL
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

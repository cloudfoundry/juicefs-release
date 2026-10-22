package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudfoundry/juicefs-release/src/broker-registration/internal/cli"
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
	configPath := os.Getenv("BROKER_REGISTRATION_CONFIG")
	if configPath == "" {
		configPath = "/var/vcap/jobs/register-broker/config/config.json"
		if os.Args[1] == "deregister" {
			configPath = "/var/vcap/jobs/deregister-broker/config/config.json"
		}
	}
	contents, err := os.ReadFile(configPath)
	if err != nil {
		fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		fatal(err)
	}
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/var/vcap/packages/cf-cli-8-linux/bin:/usr/bin:/bin")
	}
	if !strings.Contains(os.Getenv("PATH"), "/var/vcap/packages/cf-cli-8-linux/bin") {
		_ = os.Setenv("PATH", "/var/vcap/packages/cf-cli-8-linux/bin:"+os.Getenv("PATH"))
	}
	if err := runRegistration(os.Args[1], cfg, "/var/vcap/data/broker-registration", cli.ExecRunner{}); err != nil {
		fatal(err)
	}
}

func runRegistration(mode string, cfg config, dataDir string, runner cli.Runner) error {
	if mode != "register" && mode != "deregister" {
		return errors.New("invalid registration mode")
	}
	cfg.CFUsername = envOr("CF_USERNAME", cfg.CFUsername)
	cfg.CFPassword = envOr("CF_PASSWORD", cfg.CFPassword)
	cfg.BrokerUsername = envOr("BROKER_USERNAME", cfg.BrokerUsername)
	cfg.BrokerPassword = envOr("BROKER_PASSWORD", cfg.BrokerPassword)
	cfg.APIURL = envOr("CF_API_URL", cfg.APIURL)
	cfg.CACert = envOr("CF_CA_CERT", cfg.CACert)
	if strings.EqualFold(os.Getenv("BROKER_TLS_SKIP"), "true") {
		cfg.SkipBrokerSSLValidation = true
	}
	if cfg.APIURL == "" || cfg.CFUsername == "" || cfg.CFPassword == "" || cfg.BrokerName == "" || cfg.BrokerUsername == "" || cfg.BrokerPassword == "" {
		return errors.New("Cloud Controller and broker link credentials are required")
	}
	if cfg.BrokerURL == "" {
		if cfg.BrokerPort == 0 {
			return errors.New("broker URL or linked broker port is required")
		}
		cfg.BrokerURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.BrokerPort)
	}
	if strings.HasPrefix(cfg.BrokerURL, "https://") && cfg.SkipBrokerSSLValidation {
		return errors.New("cf CLI does not support skipping TLS validation for a service broker URL; provide a trusted broker certificate")
	}

	cfHome := filepath.Join(dataDir, "cf-home")
	if err := os.MkdirAll(cfHome, 0700); err != nil {
		return err
	}
	env := append(os.Environ(), "CF_HOME="+cfHome, "CF_USERNAME="+cfg.CFUsername, "CF_PASSWORD="+cfg.CFPassword)
	if cfg.CACert != "" {
		certPath := filepath.Join(dataDir, "cloud-controller-ca.crt")
		if err := os.WriteFile(certPath, []byte(cfg.CACert), 0600); err != nil {
			return err
		}
		env = append(env, "SSL_CERT_FILE="+certPath)
	}
	args := []string{"api", normalizeAPIURL(cfg.APIURL)}
	if cfg.SkipSSLValidation {
		args = append(args, "--skip-ssl-validation")
	}
	if _, err := runner.Output(env, args...); err != nil {
		return fmt.Errorf("configure Cloud Controller API: %w", err)
	}
	if _, err := runner.Output(env, "auth"); err != nil {
		return fmt.Errorf("authenticate with Cloud Controller: %w", err)
	}
	if mode == "register" {
		if err := registerBrokerWith(runner, env, cfg); err != nil {
			return err
		}
		if err := enableServiceAccessWith(runner, env, cfg.BrokerName); err != nil {
			return fmt.Errorf("enable JuiceFS service access: %w", err)
		}
	} else {
		// Preserve the existing deregistration behavior; the errand does not
		// invoke backend deletion commands.
		if _, err := runner.Output(env, "purge-service-offering", "-b", cfg.BrokerName, "juicefs", "-f"); err != nil {
			fmt.Fprintln(os.Stderr, "purge offering (may already be absent):", err)
		}
		if _, err := runner.Output(env, "delete-service-broker", cfg.BrokerName, "-f"); err != nil {
			return fmt.Errorf("delete service broker: %w", err)
		}
	}
	_, _ = runner.Output(env, "logout")
	return nil
}

func enableServiceAccessWith(runner cli.Runner, env []string, brokerName string) error {
	output, err := runner.Output(env, "curl", "/v3/service_offerings?names=juicefs")
	if err != nil {
		return err
	}
	var response struct {
		Resources []struct {
			GUID string `json:"guid"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return err
	}
	if len(response.Resources) != 1 || response.Resources[0].GUID == "" {
		return errors.New("JuiceFS service offering not found after broker registration")
	}
	_, err = runner.Output(env, "enable-service-access", "-b", brokerName, "juicefs")
	return err
}

func registerBrokerWith(runner cli.Runner, env []string, cfg config) error {
	brokers, err := runner.Output(env, "curl", "/v3/service_brokers?names="+url.QueryEscape(cfg.BrokerName))
	if err != nil {
		return err
	}
	var result struct {
		Pagination struct {
			Total int `json:"total_results"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(brokers, &result); err != nil {
		return err
	}
	if result.Pagination.Total == 0 {
		_, err = runner.Output(env, "create-service-broker", cfg.BrokerName, cfg.BrokerUsername, cfg.BrokerPassword, cfg.BrokerURL)
		return err
	}
	_, err = runner.Output(env, "update-service-broker", cfg.BrokerName, cfg.BrokerUsername, cfg.BrokerPassword, cfg.BrokerURL)
	return err
}

func normalizeAPIURL(apiURL string) string {
	if strings.HasPrefix(apiURL, "http://") || strings.HasPrefix(apiURL, "https://") {
		return apiURL
	}
	return "https://" + apiURL
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

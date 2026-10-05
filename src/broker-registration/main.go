package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
		fatal(errors.New("Cloud Controller and broker link credentials are required"))
	}
	if cfg.BrokerURL == "" {
		if cfg.BrokerPort == 0 {
			fatal(errors.New("broker URL or linked broker port is required"))
		}
		cfg.BrokerURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.BrokerPort)
	}
	if strings.HasPrefix(cfg.BrokerURL, "https://") && cfg.SkipBrokerSSLValidation {
		fatal(errors.New("cf CLI does not support skipping TLS validation for a service broker URL; provide a trusted broker certificate"))
	}

	cfHome := "/var/vcap/data/broker-registration/cf-home"
	if err := os.MkdirAll(cfHome, 0700); err != nil {
		fatal(err)
	}
	env := append(os.Environ(), "CF_HOME="+cfHome, "CF_USERNAME="+cfg.CFUsername, "CF_PASSWORD="+cfg.CFPassword)
	if cfg.CACert != "" {
		certPath := "/var/vcap/data/broker-registration/cloud-controller-ca.crt"
		if err := os.WriteFile(certPath, []byte(cfg.CACert), 0600); err != nil {
			fatal(err)
		}
		env = append(env, "SSL_CERT_FILE="+certPath)
	}
	args := []string{"api", normalizeAPIURL(cfg.APIURL)}
	if cfg.SkipSSLValidation {
		args = append(args, "--skip-ssl-validation")
	}
	if err := runCF(env, args...); err != nil {
		fatal(fmt.Errorf("configure Cloud Controller API: %w", err))
	}
	if err := runCF(env, "auth"); err != nil {
		fatal(fmt.Errorf("authenticate with Cloud Controller: %w", err))
	}
	if os.Args[1] == "register" {
		if err := registerBroker(env, cfg); err != nil {
			fatal(err)
		}
		if err := enableServiceAccess(env, cfg.BrokerName); err != nil {
			fatal(fmt.Errorf("enable JuiceFS service access: %w", err))
		}
	} else {
		// The JuiceFS offering defaults to Retain on deprovision; purge only the
		// catalog offering and broker registration, never customer directories.
		if err := runCF(env, "purge-service-offering", "-b", cfg.BrokerName, "juicefs", "-f"); err != nil {
			fmt.Fprintln(os.Stderr, "purge offering (may already be absent):", err)
		}
		if err := runCF(env, "delete-service-broker", cfg.BrokerName, "-f"); err != nil {
			fatal(fmt.Errorf("delete service broker: %w", err))
		}
	}
	_ = runCF(env, "logout")
}

func enableServiceAccess(env []string, brokerName string) error {
	output, err := cfOutput(env, "curl", "/v3/service_offerings?names=juicefs")
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
	return runCF(env, "enable-service-access", "-b", brokerName, response.Resources[0].GUID)
}

func registerBroker(env []string, cfg config) error {
	brokers, err := cfOutput(env, "curl", "/v2/service_brokers?q=name:"+cfg.BrokerName)
	if err != nil {
		return err
	}
	var result struct {
		Total int `json:"total_results"`
	}
	if err := json.Unmarshal(brokers, &result); err != nil {
		return err
	}
	if result.Total == 0 {
		return runCF(env, "create-service-broker", cfg.BrokerName, cfg.BrokerUsername, cfg.BrokerPassword, cfg.BrokerURL)
	}
	return runCF(env, "update-service-broker", cfg.BrokerName, cfg.BrokerUsername, cfg.BrokerPassword, cfg.BrokerURL)
}

func cfOutput(env []string, args ...string) ([]byte, error) {
	cfPath, err := exec.LookPath("cf")
	if err != nil {
		return nil, fmt.Errorf("locate vendored cf CLI: %w", err)
	}
	cmd := exec.Command(cfPath, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("cf %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func runCF(env []string, args ...string) error {
	_, err := cfOutput(env, args...)
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

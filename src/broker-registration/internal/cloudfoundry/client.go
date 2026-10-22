// Package cloudfoundry adapts go-cfclient to the broker registration lifecycle.
package cloudfoundry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudfoundry/go-cfclient/v3/client"
	"github.com/cloudfoundry/go-cfclient/v3/config"
	"github.com/cloudfoundry/go-cfclient/v3/resource"
)

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate
//counterfeiter:generate -o ../../fakes/api.go . API
type API interface {
	Brokers(context.Context, string) ([]*resource.ServiceBroker, error)
	CreateBroker(context.Context, *resource.ServiceBrokerCreate) error
	UpdateBroker(context.Context, string, *resource.ServiceBrokerUpdate) error
	DeleteBroker(context.Context, string) error
	Offerings(context.Context, string) ([]*resource.ServiceOffering, error)
	Plans(context.Context, string) ([]*resource.ServicePlan, error)
	EnablePlan(context.Context, string) error
	DeletePlan(context.Context, string) error
	DeleteOffering(context.Context, string) error
}

type Client struct{ cf *client.Client }

func New(endpoint, username, password, ca string, skipTLS bool) (*Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load CA roots: %w", err)
	}
	if ca != "" && !roots.AppendCertsFromPEM([]byte(ca)) {
		return nil, fmt.Errorf("invalid Cloud Controller CA certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	opts := []config.Option{config.UserPassword(username, password), config.HttpClient(&http.Client{Transport: transport}), config.RequestTimeout(30 * time.Second)}
	if skipTLS {
		opts = append(opts, config.SkipTLSValidation())
	}
	cfg, err := config.New(endpoint, opts...)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("authenticate with Cloud Controller: %w", err)
	}
	cf, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cf: cf}, nil
}

func (c *Client) Brokers(ctx context.Context, name string) ([]*resource.ServiceBroker, error) {
	opts := client.NewServiceBrokerListOptions()
	opts.Names.EqualTo(name)
	return c.cf.ServiceBrokers.ListAll(ctx, opts)
}
func (c *Client) wait(ctx context.Context, job string, err error) error {
	if err != nil {
		return err
	}
	if job == "" {
		return nil
	}
	return c.cf.Jobs.PollComplete(ctx, job, client.NewPollingOptions())
}
func (c *Client) CreateBroker(ctx context.Context, request *resource.ServiceBrokerCreate) error {
	job, err := c.cf.ServiceBrokers.Create(ctx, request)
	return c.wait(ctx, job, err)
}
func (c *Client) UpdateBroker(ctx context.Context, guid string, request *resource.ServiceBrokerUpdate) error {
	job, _, err := c.cf.ServiceBrokers.Update(ctx, guid, request)
	return c.wait(ctx, job, err)
}
func (c *Client) DeleteBroker(ctx context.Context, guid string) error {
	job, err := c.cf.ServiceBrokers.Delete(ctx, guid)
	return c.wait(ctx, job, err)
}
func (c *Client) Offerings(ctx context.Context, brokerGUID string) ([]*resource.ServiceOffering, error) {
	opts := client.NewServiceOfferingListOptions()
	opts.ServiceBrokerGUIDs.EqualTo(brokerGUID)
	opts.Names.EqualTo("juicefs")
	return c.cf.ServiceOfferings.ListAll(ctx, opts)
}
func (c *Client) Plans(ctx context.Context, offeringGUID string) ([]*resource.ServicePlan, error) {
	opts := client.NewServicePlanListOptions()
	opts.ServiceOfferingGUIDs.EqualTo(offeringGUID)
	return c.cf.ServicePlans.ListAll(ctx, opts)
}
func (c *Client) EnablePlan(ctx context.Context, guid string) error {
	_, err := c.cf.ServicePlansVisibility.Apply(ctx, guid, resource.NewServicePlanVisibilityUpdate(resource.ServicePlanVisibilityPublic))
	return err
}
func (c *Client) DeletePlan(ctx context.Context, guid string) error {
	return c.cf.ServicePlans.Delete(ctx, guid)
}
func (c *Client) DeleteOffering(ctx context.Context, guid string) error {
	return c.cf.ServiceOfferings.Delete(ctx, guid)
}

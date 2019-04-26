package sentry

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"time"
)

var debugger = log.New(ioutil.Discard, "[Sentry]", log.LstdFlags)

type Integration interface {
	Name() string
	SetupOnce()
}

type ClientOptions struct {
	Dsn              string
	Debug            bool
	SampleRate       float32
	BeforeSend       func(event *Event, hint *EventHint) *Event
	BeforeBreadcrumb func(breadcrumb *Breadcrumb, hint *BreadcrumbHint) *Breadcrumb
	Integrations     []Integration
	Transport        Transport
	ServerName       string
	Release          string
	Dist             string
	Environment      string
	MaxBreadcrumbs   int
	DebugWriter      io.Writer
}

type Client struct {
	options      ClientOptions
	dsn          *Dsn
	integrations map[string]Integration
	Transport    Transport
}

// Or client.Configure which would allow us to keep most data on struct private
func NewClient(options ClientOptions) (*Client, error) {
	if options.Debug {
		debugWriter := options.DebugWriter
		if debugWriter == nil {
			debugWriter = os.Stdout
		}
		debugger.SetOutput(debugWriter)
	}

	if options.Dsn == "" {
		options.Dsn = os.Getenv("SENTRY_DSN")
	}

	if options.Release == "" {
		options.Release = os.Getenv("SENTRY_RELEASE")
	}

	if options.Environment == "" {
		options.Environment = os.Getenv("SENTRY_ENVIRONMENT")
	}

	dsn, err := NewDsn(options.Dsn)

	if err != nil {
		return nil, err
	}

	if dsn == nil {
		debugger.Println("Sentry client initialized with an empty DSN")
	}

	client := Client{
		options: options,
		dsn:     dsn,
	}

	client.setupTransport()
	client.setupIntegrations()

	return &client, nil
}

func (client *Client) setupTransport() {
	transport := client.options.Transport

	if transport == nil {
		transport = new(HTTPTransport)
	}

	transport.Configure(client.options)
	client.Transport = transport
}

func (client *Client) setupIntegrations() {
	if client.options.Integrations == nil {
		return
	}

	client.integrations = make(map[string]Integration)

	for _, integration := range client.options.Integrations {
		client.integrations[integration.Name()] = integration
		integration.SetupOnce()
		debugger.Printf("Integration installed: %s\n", integration.Name())
	}
}

func (client Client) Options() ClientOptions {
	return client.options
}

func (client *Client) CaptureMessage(message string, hint *EventHint, scope EventModifier) {
	event := client.eventFromMessage(message)
	client.CaptureEvent(event, hint, scope)
}

func (client *Client) CaptureException(exception error, hint *EventHint, scope EventModifier) {
	event := client.eventFromException(exception)
	client.CaptureEvent(event, hint, scope)
}

func (client *Client) CaptureEvent(event *Event, hint *EventHint, scope EventModifier) {
	// TODO: Handle return values
	if _, err := client.processEvent(event, hint, scope); err != nil {
		debugger.Println(err)
	}
}

func (client *Client) Recover(recoveredErr interface{}, scope *Scope) {
	if recoveredErr == nil {
		recoveredErr = recover()
	}

	if recoveredErr != nil {
		if err, ok := recoveredErr.(error); ok {
			CaptureException(err)
		}

		if err, ok := recoveredErr.(string); ok {
			CaptureMessage(err)
		}
	}
}

func (client *Client) RecoverWithContext(ctx context.Context, recoveredErr interface{}, scope *Scope) {
	if recoveredErr == nil {
		recoveredErr = recover()
	}

	if recoveredErr != nil {
		var currentHub *Hub

		if HasHubOnContext(ctx) {
			currentHub = GetHubFromContext(ctx)
		} else {
			currentHub = CurrentHub()
		}

		if err, ok := recoveredErr.(error); ok {
			currentHub.CaptureException(err, &EventHint{
				Context: ctx,
			})
		}

		if err, ok := recoveredErr.(string); ok {
			currentHub.CaptureMessage(err, &EventHint{
				Context: ctx,
			})
		}
	}
}

func (client *Client) eventFromMessage(message string) *Event {
	return &Event{
		Message: message,
	}
}

func (client *Client) eventFromException(exception error) *Event {
	// TODO: Extract stacktrace from the exception
	return &Event{
		Message: exception.Error(),
	}
}

// TODO: Should return some sort of SentryResponse instead of http.Response
func (client *Client) processEvent(event *Event, hint *EventHint, scope EventModifier) (*http.Response, error) {
	options := client.Options()

	// TODO: Reconsider if its worth going away from default implementation
	// of other SDKs. In Go zero value (default) for float32 is 0.0,
	// which means that if someone uses ClientOptions{} struct directly
	// and we would not check for 0 here, we'd skip all events by default
	if options.SampleRate != 0.0 {
		randomFloat := rand.New(rand.NewSource(time.Now().UnixNano())).Float32()
		if randomFloat > options.SampleRate {
			return nil, fmt.Errorf("event dropped due to SampleRate hit")
		}
	}

	if event = client.prepareEvent(event, hint, scope); event == nil {
		return nil, fmt.Errorf("event dropped by one of the EventProcessors")
	}

	if options.BeforeSend != nil {
		h := &EventHint{}
		if hint != nil {
			h = hint
		}
		if event = options.BeforeSend(event, h); event == nil {
			return nil, fmt.Errorf("event dropped due to BeforeSend callback")
		}
	}

	return client.Transport.SendEvent(event)
}

func (client *Client) prepareEvent(event *Event, hint *EventHint, scope EventModifier) *Event {
	// TODO: Set all the defaults, clear unnecessary stuff etc. here

	if event.EventID == "" {
		event.EventID = uuid()
	}

	if event.Timestamp == 0 {
		event.Timestamp = time.Now().Unix()
	}

	if event.Level == "" {
		event.Level = LevelInfo
	}

	if event.ServerName == "" {
		if hostname, err := os.Hostname(); err == nil {
			event.ServerName = hostname
		}
	}

	event.Sdk = SdkInfo{
		Name:         "sentry.go",
		Version:      VERSION,
		Integrations: client.listIntegrations(),
		Packages: []SdkPackage{{
			Name:    "sentry-go",
			Version: VERSION,
		}},
	}
	event.Platform = "go"
	event.Transaction = "Don't sneak into my computer please"

	return scope.ApplyToEvent(event, hint)
}

func (client Client) listIntegrations() []string {
	integrations := make([]string, 0, len(client.integrations))
	for key := range client.integrations {
		integrations = append(integrations, key)
	}
	sort.Strings(integrations)
	return integrations
}

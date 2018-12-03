package main

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/crewjam/saml/samlsp"
	log "github.com/sirupsen/logrus"

	"github.com/vulcand/oxy/buffer"
	"github.com/vulcand/oxy/cbreaker"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/ratelimit"
	"github.com/vulcand/oxy/roundrobin"
	"github.com/vulcand/oxy/trace"
	"github.com/vulcand/oxy/utils"

	"github.com/labstack/echo"
)

// Config for reverse proxy settings and RBAC users and groups
type Config struct {
	ListenInterface        string        `yaml:"listen_interface"`
	ListenPort             int           `yaml:"listen_port"`
	Targets                []string      `yaml:"targets"`
	IdpMetadataURL         string        `yaml:"idp_metadata_url"`
	ServiceRootURL         string        `yaml:"service_root_url"`
	CertPath               string        `yaml:"cert_path"`
	KeyPath                string        `yaml:"key_path"`
	RateLimitAvgSecond     int64         `yaml:"rate_limit_avg_second"`
	RateLimitBurstSecond   int64         `yaml:"rate_limit_burst_second"`
	TraceRequestHeaders    []string      `yaml:"trace_request_headers"`
	AddAttributesAsHeaders []string      `yaml:"add_attributes_as_headers"`
	CookieMaxAge           time.Duration `yaml:"cookie_max_age"`
	LogLevel               string        `yaml:"log_level"`
	ClientIPSource         string        `yaml:"client_ip_source"`
}
type server struct {
	config Config
}

func (C *Config) getConf(configPath string) {
	yamlFile, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.WithFields(log.Fields{
			"config_path": configPath,
			"error":       err.Error()}).Fatal("could not read config")
	}
	err = yaml.Unmarshal(yamlFile, C)
	if err != nil {
		log.WithFields(log.Fields{
			"config_path": configPath,
			"error":       err.Error()}).Fatal("could not parse config")
	}
}

func newServer() *server {
	var configPath string
	flag.StringVar(&configPath, "c", "config.yaml", "path to the config file")
	flag.Parse()
	var C Config
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		log.WithFields(log.Fields{
			"config_path": configPath,
			"error":       err.Error()}).Fatal("could not determine absolute path for config")
	}
	C.getConf(absPath)

	log.Print("config loaded")
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)
	logLevel, err := log.ParseLevel(C.LogLevel)
	if err != nil {
		log.WithFields(log.Fields{
			"log_level": C.LogLevel,
			"error":     err.Error()}).Fatal("could not parse log level")
	}
	log.SetLevel(logLevel)

	s := server{config: C}
	return &s
}

func (s *server) addSamlHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attributes := samlsp.Token(r.Context()).Attributes
		for _, attr := range s.config.AddAttributesAsHeaders {
			if val, ok := attributes[attr]; ok {
				r.Header.Add("X-Saml-"+attr, strings.Join(val, " "))
			} else {
				log.WithFields(log.Fields{"attrs_available": attributes,
					"attr": attr}).Warn("given attr not in attributes")
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) getMiddleware() http.Handler {
	// reverse proxy layer
	fwd, err := forward.New()
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize reverse proxy middleware")
	}

	// rate-limiting layers
	var extractorSource = s.config.ClientIPSource
	extractor, err := utils.NewExtractor(extractorSource)
	if err != nil {
		log.WithFields(log.Fields{"extractor": extractorSource,
			"error": err.Error()}).Fatal("could not use given rate limiting extractor")
	}
	rates := ratelimit.NewRateSet()
	err = rates.Add(time.Second, s.config.RateLimitAvgSecond, s.config.RateLimitBurstSecond)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error()}).Fatal("could not set rate limiting rates")
	}
	rm, err := ratelimit.New(fwd, extractor, rates)
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize rate limiter middleware")
	}

	// circuit-breaker layer
	const triggerNetRatio = `NetworkErrorRatio() > 0.5`
	cb, err := cbreaker.New(rm, triggerNetRatio)
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize circuit breaker middleware")
	}

	// load balancing layer
	lb, err := roundrobin.New(cb)
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize load balancing middleware")
	}

	var targetURL *url.URL
	for _, target := range s.config.Targets {
		targetURL, err = url.Parse(target)
		if err != nil {
			log.WithFields(log.Fields{
				"target": target,
				"error":  err.Error()}).Fatal("could not parse target")
		}
		// add target to the load balancer
		err = lb.UpsertServer(targetURL)
		if err != nil {
			log.WithFields(log.Fields{
				"target": target,
				"error":  err.Error()}).Fatal("could not add target to load balancer")
		}
	}

	// trace layer
	trace, err := trace.New(lb, io.Writer(os.Stdout),
		trace.Option(trace.RequestHeaders(s.config.TraceRequestHeaders...)))
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize request tracing middleware")
	}

	// buffer will read the request body and will replay the request again in case if forward returned status
	// corresponding to nework error (e.g. Gateway Timeout)
	buffer, err := buffer.New(trace, buffer.Retry(`IsNetworkError() && Attempts() < 3`))
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize buffering middleware")
	}

	return buffer
}

func main() {
	s := newServer()

	keyPair, err := tls.LoadX509KeyPair(s.config.CertPath, s.config.KeyPath)
	if err != nil {
		log.WithFields(log.Fields{
			"cert_path": s.config.CertPath,
			"key_path":  s.config.KeyPath,
			"error":     err.Error()}).Fatal("could not load keypair")
	}
	keyPair.Leaf, err = x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		log.WithFields(log.Fields{
			"cert_path": s.config.CertPath,
			"error":     err.Error()}).Fatal("could not parse certificate")
	}

	idpMetadataURL, err := url.Parse(s.config.IdpMetadataURL)
	if err != nil {
		log.WithFields(log.Fields{
			"idp_metadata_url": s.config.IdpMetadataURL,
			"error":            err.Error()}).Fatal("could not parse metadata URL")
	}

	rootURL, err := url.Parse(s.config.ServiceRootURL)
	if err != nil {
		log.WithFields(log.Fields{
			"service_root_url": s.config.IdpMetadataURL,
			"error":            err.Error()}).Fatal("could not parse service root URL")
	}

	// initialize SAML middleware
	samlSP, err := samlsp.New(samlsp.Options{
		URL:            *rootURL,
		Key:            keyPair.PrivateKey.(*rsa.PrivateKey),
		Certificate:    keyPair.Leaf,
		IDPMetadataURL: idpMetadataURL,
		CookieMaxAge:   s.config.CookieMaxAge,
	})
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Fatal("could not initialize SAML middleware")
	}

	// Use mux for explicit paths and so no other routes are accidently exposed
	router := echo.New()

	// This endpoint handles SAML auth flow
	router.Any("/saml/*", echo.WrapHandler(samlSP))
	// These endpoints require valid session cookie
	router.Any("/*", echo.WrapHandler(samlSP.RequireAccount(s.addSamlHeaders(s.getMiddleware()))), NoCache())

	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.config.ListenInterface, s.config.ListenPort),
		Handler: router,
		// This breaks streaming requests
		ReadTimeout: 45 * time.Second,
		// This breaks long downloads
		WriteTimeout: 45 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	log.Fatal(srv.ListenAndServe())
}

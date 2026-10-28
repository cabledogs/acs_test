// cpesim simulates many TR-069 (CWMP) CPEs checking in to an ACS, for load testing.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ACSURL, User, Pass                     string
	Count, StartIndex, SerialDigits        int
	SerialPrefix, OUI, ProductClass        string
	Manufacturer, ModelName                string
	SWVersion, HWVersion, DataModel        string
	CWMPNS                                 string
	Interval, Ramp, Duration, Timeout      time.Duration
	BootDelay, Report                      time.Duration
	Jitter                                 float64
	Bootstrap, Once, Insecure              bool
	SharedConns, FetchDownloads, LogErrors bool
	MaxSessions, ExtraParams, Trace        int
	CRPort                                 int
	CRHost, CSV, BindIPs                   string
}

type Sim struct {
	cfg      *Config
	stats    *Stats
	sem      chan struct{}
	sharedTr *http.Transport
	bindIPs  []net.IP
	crHost   string
	cpes     map[string]*CPE
}

func main() {
	cfg := &Config{}
	flag.StringVar(&cfg.ACSURL, "acs", "", "ACS URL, e.g. http://acs.example.com:7547/ (required)")
	flag.StringVar(&cfg.User, "user", "", "ACS username (Basic or Digest, picked from the ACS challenge)")
	flag.StringVar(&cfg.Pass, "pass", "", "ACS password")
	flag.IntVar(&cfg.Count, "n", 100, "number of simulated CPEs")
	flag.IntVar(&cfg.StartIndex, "start", 1, "first serial number index")
	flag.IntVar(&cfg.SerialDigits, "serial-digits", 8, "zero-padded digits in the serial number")
	flag.StringVar(&cfg.SerialPrefix, "serial-prefix", "SIM", "serial number prefix")
	flag.StringVar(&cfg.OUI, "oui", "001122", "ManufacturerOUI")
	flag.StringVar(&cfg.ProductClass, "product-class", "LoadTestCPE", "ProductClass")
	flag.StringVar(&cfg.Manufacturer, "manufacturer", "SimVendor", "Manufacturer")
	flag.StringVar(&cfg.ModelName, "model", "SIM-1000", "ModelName")
	flag.StringVar(&cfg.SWVersion, "sw-version", "1.0.0", "SoftwareVersion")
	flag.StringVar(&cfg.HWVersion, "hw-version", "1.0", "HardwareVersion")
	flag.StringVar(&cfg.DataModel, "datamodel", "tr181", "data model: tr181 (Device.) or tr098 (InternetGatewayDevice.)")
	flag.StringVar(&cfg.CWMPNS, "cwmp-ns", defaultCWMPNS, "CWMP namespace used for Inform")
	flag.IntVar(&cfg.ExtraParams, "extra-params", 0, "add N extra dummy parameters per CPE (bigger GetParameterValues payloads)")
	flag.DurationVar(&cfg.Interval, "interval", 5*time.Minute, "periodic inform interval (ACS may change it via SetParameterValues)")
	flag.Float64Var(&cfg.Jitter, "jitter", 0.1, "random +/- fraction applied to the periodic interval")
	flag.DurationVar(&cfg.Ramp, "ramp", time.Minute, "spread the first informs of all CPEs evenly over this duration (0 = all at once)")
	flag.DurationVar(&cfg.Duration, "duration", 0, "stop after this long (0 = run until Ctrl+C)")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "per HTTP request timeout")
	flag.DurationVar(&cfg.BootDelay, "boot-delay", 5*time.Second, "simulated reboot time after Reboot/FactoryReset/firmware Download")
	flag.BoolVar(&cfg.Bootstrap, "bootstrap", true, "send 0 BOOTSTRAP in the first Inform")
	flag.BoolVar(&cfg.Once, "once", false, "each CPE does a single session and exits (burst test)")
	flag.IntVar(&cfg.MaxSessions, "max-sessions", 0, "max concurrent sessions (0 = unlimited)")
	flag.BoolVar(&cfg.Insecure, "insecure", false, "skip TLS certificate verification")
	flag.BoolVar(&cfg.SharedConns, "shared-conns", false, "share a keep-alive connection pool across CPEs (fewer sockets, less realistic)")
	flag.StringVar(&cfg.BindIPs, "bind-ips", "", "comma separated local source IPs to spread connections over")
	flag.BoolVar(&cfg.FetchDownloads, "fetch-downloads", false, "actually download files requested by Download RPCs")
	flag.IntVar(&cfg.CRPort, "cr-port", 0, "listen for connection requests on this port (0 = disabled), URL is http://<cr-host>:<port>/<serial>")
	flag.StringVar(&cfg.CRHost, "cr-host", "", "host/IP put in ConnectionRequestURL (default: local IP used to reach the ACS)")
	flag.DurationVar(&cfg.Report, "report", 5*time.Second, "stats reporting interval")
	flag.StringVar(&cfg.CSV, "csv", "", "also write periodic stats to this CSV file")
	flag.BoolVar(&cfg.LogErrors, "log-errors", false, "log every failed session")
	flag.IntVar(&cfg.Trace, "trace", -1, "dump all SOAP traffic of CPE number N (0-based)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "cpesim - TR-069 CPE load simulator\n\nUsage:\n  cpesim -acs http://acs:7547/ -n 1000 [options]\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if cfg.ACSURL == "" {
		flag.Usage()
		os.Exit(2)
	}
	u, err := url.Parse(cfg.ACSURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Fatalf("invalid -acs URL %q", cfg.ACSURL)
	}
	if cfg.DataModel != "tr181" && cfg.DataModel != "tr098" {
		log.Fatalf("-datamodel must be tr181 or tr098")
	}
	if cfg.Count < 1 || cfg.Interval < time.Second {
		log.Fatalf("-n must be >= 1 and -interval >= 1s")
	}

	sim := &Sim{cfg: cfg, stats: newStats(), cpes: map[string]*CPE{}}
	for _, s := range strings.Split(cfg.BindIPs, ",") {
		if s = strings.TrimSpace(s); s != "" {
			ip := net.ParseIP(s)
			if ip == nil {
				log.Fatalf("bad IP in -bind-ips: %q", s)
			}
			sim.bindIPs = append(sim.bindIPs, ip)
		}
	}
	if cfg.MaxSessions > 0 {
		sim.sem = make(chan struct{}, cfg.MaxSessions)
	}
	if cfg.SharedConns {
		sim.sharedTr = sim.makeTransport(0)
		sim.sharedTr.MaxIdleConnsPerHost = 10000
	}
	if cfg.CRPort > 0 {
		sim.crHost = cfg.CRHost
		if sim.crHost == "" {
			sim.crHost = localIPFor(u)
		}
	}

	cpes := make([]*CPE, cfg.Count)
	for i := range cpes {
		cpes[i] = newCPE(sim, i)
		sim.cpes[cpes[i].serial] = cpes[i]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	if cfg.CRPort > 0 {
		go sim.serveConnectionRequests(ctx)
	}

	log.Printf("ACS %s | %d CPEs (%s..%s) | datamodel %s | interval %s | ramp %s",
		cfg.ACSURL, cfg.Count, cpes[0].serial, cpes[len(cpes)-1].serial, cfg.DataModel, cfg.Interval, cfg.Ramp)
	if !cfg.Once {
		log.Printf("expected steady-state load: %.1f informs/sec", float64(cfg.Count)/cfg.Interval.Seconds())
	}
	if cfg.CRPort > 0 {
		log.Printf("connection requests: http://%s:%d/<serial>", sim.crHost, cfg.CRPort)
	}
	log.Printf("press Ctrl+C to stop")

	var wg sync.WaitGroup
	for i, c := range cpes {
		var delay time.Duration
		if cfg.Ramp > 0 {
			delay = time.Duration(int64(cfg.Ramp) * int64(i) / int64(cfg.Count))
		}
		wg.Add(1)
		go func(c *CPE, d time.Duration) {
			defer wg.Done()
			c.run(ctx, d)
		}(c, delay)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	sim.stats.report(ctx, done, cfg)
}

func (sim *Sim) makeTransport(idx int) *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if len(sim.bindIPs) > 0 {
		d.LocalAddr = &net.TCPAddr{IP: sim.bindIPs[idx%len(sim.bindIPs)]}
	}
	return &http.Transport{
		Proxy:               nil,
		DialContext:         d.DialContext,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: sim.cfg.Insecure},
		TLSHandshakeTimeout: 15 * time.Second,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     60 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // CWMP is HTTP/1.1
	}
}

func (sim *Sim) crURL(c *CPE) string {
	if sim.cfg.CRPort > 0 {
		return fmt.Sprintf("http://%s:%d/%s", sim.crHost, sim.cfg.CRPort, c.serial)
	}
	return fmt.Sprintf("http://%s:7547/", c.ip)
}

func (sim *Sim) runSession(ctx context.Context, c *CPE) error {
	if sim.sem != nil {
		select {
		case sim.sem <- struct{}{}:
			defer func() { <-sim.sem }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	st := sim.stats
	st.active.Add(1)
	defer st.active.Add(-1)
	st.started.Add(1)
	start := time.Now()
	s := sim.newSession(c)
	defer s.close()
	err := s.run(ctx)
	switch {
	case err == nil:
		st.ok.Add(1)
		st.sessionLat.add(time.Since(start))
	case ctx.Err() != nil:
		st.aborted.Add(1)
	default:
		st.failed.Add(1)
		st.recordError(err)
		if sim.cfg.LogErrors {
			log.Printf("[%s] session failed: %v", c.serial, err)
		}
	}
	return err
}

func (sim *Sim) serveConnectionRequests(ctx context.Context) {
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", sim.cfg.CRPort),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := sim.cpes[strings.Trim(r.URL.Path, "/")]
			if c == nil {
				http.NotFound(w, r)
				return
			}
			sim.stats.crReceived.Add(1)
			select {
			case c.crCh <- struct{}{}:
			default: // a connection request is already pending
			}
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { <-ctx.Done(); srv.Close() }()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("connection request listener: %v", err)
	}
}

// localIPFor returns the local IP the OS would use to reach the ACS.
func localIPFor(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, port))
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

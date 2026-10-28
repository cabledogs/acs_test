package main

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

type param struct {
	value    string
	typ      string
	writable bool
}

type event struct{ code, key string }

// transfer is a finished Download waiting to be reported via TransferComplete.
type transfer struct {
	key             string
	start, complete time.Time
	faultCode       int
	faultString     string
}

type download struct {
	key, fileType, url, user, pass string
	delay                          time.Duration
}

// CPE holds the state of one simulated device. All fields except crCh are
// only touched by the CPE's own goroutine.
type CPE struct {
	sim                   *Sim
	idx                   int
	serial, root, ip, mac string
	params                map[string]*param
	extraObjs             map[string]bool
	bootTime              time.Time
	interval              time.Duration
	periodicEnabled       bool
	events                []event
	retry                 int
	msgID                 int
	transfer              *transfer
	pendingDL             *download
	rebootKey             *string
	factoryReset          bool
	scheduleAt            time.Time
	scheduleKey           string
	crCh                  chan struct{}
}

func newCPE(sim *Sim, idx int) *CPE {
	cfg := sim.cfg
	n := idx + 1
	c := &CPE{
		sim:      sim,
		idx:      idx,
		serial:   fmt.Sprintf("%s%0*d", cfg.SerialPrefix, cfg.SerialDigits, cfg.StartIndex+idx),
		ip:       fmt.Sprintf("10.%d.%d.%d", (n>>16)&255, (n>>8)&255, n&255),
		mac:      fmt.Sprintf("02:00:%02X:%02X:%02X:%02X", (n>>24)&255, (n>>16)&255, (n>>8)&255, n&255),
		bootTime: time.Now(),
		crCh:     make(chan struct{}, 1),
	}
	if cfg.DataModel == "tr098" {
		c.root = "InternetGatewayDevice."
	} else {
		c.root = "Device."
	}
	c.initParams()
	return c
}

func (c *CPE) initParams() {
	cfg := c.sim.cfg
	c.params = map[string]*param{}
	c.extraObjs = map[string]bool{}
	c.interval = cfg.Interval
	c.periodicEnabled = true
	const s, u, b, dt = "xsd:string", "xsd:unsignedInt", "xsd:boolean", "xsd:dateTime"
	set := func(name, typ, val string, w bool) { c.params[c.root+name] = &param{val, typ, w} }

	set("DeviceInfo.Manufacturer", s, cfg.Manufacturer, false)
	set("DeviceInfo.ManufacturerOUI", s, cfg.OUI, false)
	set("DeviceInfo.ModelName", s, cfg.ModelName, false)
	set("DeviceInfo.Description", s, "Simulated CPE", false)
	set("DeviceInfo.ProductClass", s, cfg.ProductClass, false)
	set("DeviceInfo.SerialNumber", s, c.serial, false)
	set("DeviceInfo.HardwareVersion", s, cfg.HWVersion, false)
	set("DeviceInfo.SoftwareVersion", s, cfg.SWVersion, false)
	set("DeviceInfo.ProvisioningCode", s, "", true)
	set("DeviceInfo.UpTime", u, "0", false)
	set("ManagementServer.URL", s, cfg.ACSURL, true)
	set("ManagementServer.Username", s, cfg.User, true)
	set("ManagementServer.Password", s, "", true)
	set("ManagementServer.PeriodicInformEnable", b, "true", true)
	set("ManagementServer.PeriodicInformInterval", u, strconv.Itoa(int(cfg.Interval.Seconds())), true)
	set("ManagementServer.PeriodicInformTime", dt, "0001-01-01T00:00:00Z", true)
	set("ManagementServer.ParameterKey", s, "", false)
	set("ManagementServer.ConnectionRequestURL", s, c.sim.crURL(c), false)
	set("ManagementServer.ConnectionRequestUsername", s, c.serial, true)
	set("ManagementServer.ConnectionRequestPassword", s, "", true)
	set("Time.NTPServer1", s, "pool.ntp.org", true)

	if cfg.DataModel == "tr098" {
		set("DeviceSummary", s, "InternetGatewayDevice:1.4[](Baseline:1, EthernetLAN:1, WiFiLAN:1)", false)
		wan := "WANDevice.1.WANConnectionDevice.1.WANIPConnection.1."
		set(wan+"Enable", b, "true", true)
		set(wan+"ConnectionStatus", s, "Connected", false)
		set(wan+"ExternalIPAddress", s, c.ip, false)
		set(wan+"MACAddress", s, c.mac, false)
		set("LANDevice.1.WLANConfiguration.1.Enable", b, "true", true)
		set("LANDevice.1.WLANConfiguration.1.SSID", s, "SIM-"+c.serial, true)
		set("LANDevice.1.WLANConfiguration.1.PreSharedKey.1.KeyPassphrase", s, "", true)
		set("LANDevice.1.Hosts.HostNumberOfEntries", u, "0", false)
	} else {
		set("RootDataModelVersion", s, "2.12", false)
		set("IP.Interface.1.Enable", b, "true", true)
		set("IP.Interface.1.Status", s, "Up", false)
		set("IP.Interface.1.Name", s, "wan0", false)
		set("IP.Interface.1.IPv4Address.1.IPAddress", s, c.ip, true)
		set("IP.Interface.1.IPv4Address.1.SubnetMask", s, "255.0.0.0", true)
		set("IP.Interface.1.IPv4Address.1.AddressingType", s, "DHCP", false)
		set("Ethernet.Interface.1.MACAddress", s, c.mac, false)
		set("WiFi.SSID.1.Enable", b, "true", true)
		set("WiFi.SSID.1.SSID", s, "SIM-"+c.serial, true)
		set("WiFi.AccessPoint.1.Security.KeyPassphrase", s, "", true)
		set("Hosts.HostNumberOfEntries", u, "0", false)
	}
	for i := 1; i <= cfg.ExtraParams; i++ {
		set(fmt.Sprintf("X_LOADTEST.Param.%d", i), s, fmt.Sprintf("value-%d-%s", i, c.serial), true)
	}
}

func (c *CPE) informParamNames() []string {
	if c.sim.cfg.DataModel == "tr098" {
		return []string{"DeviceSummary", "DeviceInfo.HardwareVersion", "DeviceInfo.SoftwareVersion",
			"DeviceInfo.ProvisioningCode", "ManagementServer.ConnectionRequestURL", "ManagementServer.ParameterKey",
			"WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.ExternalIPAddress"}
	}
	return []string{"RootDataModelVersion", "DeviceInfo.HardwareVersion", "DeviceInfo.SoftwareVersion",
		"DeviceInfo.ProvisioningCode", "ManagementServer.ConnectionRequestURL", "ManagementServer.ParameterKey",
		"IP.Interface.1.IPv4Address.1.IPAddress"}
}

func (c *CPE) value(name string, p *param) string {
	if strings.HasSuffix(name, "DeviceInfo.UpTime") {
		return strconv.Itoa(int(time.Since(c.bootTime).Seconds()))
	}
	return p.value
}

func (c *CPE) sortedParams() []string {
	names := make([]string, 0, len(c.params))
	for k := range c.params {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// objects returns every object path (ending in ".") implied by the parameters
// plus objects created empty by AddObject.
func (c *CPE) objects() map[string]bool {
	o := map[string]bool{}
	add := func(name string) {
		for i := 0; i < len(name); i++ {
			if name[i] == '.' {
				o[name[:i+1]] = true
			}
		}
	}
	for k := range c.params {
		add(k)
	}
	for k := range c.extraObjs {
		add(k)
	}
	return o
}

func (c *CPE) addEvent(code, key string) {
	for _, e := range c.events {
		if e.code == code {
			return
		}
	}
	c.events = append(c.events, event{code, key})
}

func (c *CPE) nextID() string {
	c.msgID++
	return strconv.Itoa(c.msgID)
}

func pvs(name, typ, val string) string {
	return "<ParameterValueStruct><Name>" + esc(name) + `</Name><Value xsi:type="` + typ + `">` + esc(val) + "</Value></ParameterValueStruct>"
}

func (c *CPE) buildInform() string {
	cfg := c.sim.cfg
	events := c.events
	if len(events) == 0 {
		events = []event{{"2 PERIODIC", ""}}
	}
	var b strings.Builder
	b.WriteString("<cwmp:Inform><DeviceId>")
	fmt.Fprintf(&b, "<Manufacturer>%s</Manufacturer><OUI>%s</OUI><ProductClass>%s</ProductClass><SerialNumber>%s</SerialNumber>",
		esc(cfg.Manufacturer), esc(cfg.OUI), esc(cfg.ProductClass), esc(c.serial))
	fmt.Fprintf(&b, `</DeviceId><Event soap-enc:arrayType="cwmp:EventStruct[%d]">`, len(events))
	for _, e := range events {
		fmt.Fprintf(&b, "<EventStruct><EventCode>%s</EventCode><CommandKey>%s</CommandKey></EventStruct>", esc(e.code), esc(e.key))
	}
	fmt.Fprintf(&b, "</Event><MaxEnvelopes>1</MaxEnvelopes><CurrentTime>%s</CurrentTime><RetryCount>%d</RetryCount>",
		time.Now().Format(time.RFC3339), c.retry)
	names := c.informParamNames()
	fmt.Fprintf(&b, `<ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[%d]">`, len(names))
	for _, n := range names {
		full := c.root + n
		if p := c.params[full]; p != nil {
			b.WriteString(pvs(full, p.typ, c.value(full, p)))
		}
	}
	b.WriteString("</ParameterList></cwmp:Inform>")
	return b.String()
}

func (c *CPE) buildTransferComplete() string {
	t := c.transfer
	return fmt.Sprintf("<cwmp:TransferComplete><CommandKey>%s</CommandKey><FaultStruct><FaultCode>%d</FaultCode><FaultString>%s</FaultString></FaultStruct><StartTime>%s</StartTime><CompleteTime>%s</CompleteTime></cwmp:TransferComplete>",
		esc(t.key), t.faultCode, esc(t.faultString), t.start.UTC().Format(time.RFC3339), t.complete.UTC().Format(time.RFC3339))
}

// run is the CPE's life: initial delay, then sessions forever (or once).
func (c *CPE) run(ctx context.Context, startDelay time.Duration) {
	cfg := c.sim.cfg
	if !sleepCtx(ctx, startDelay) {
		return
	}
	if cfg.Bootstrap {
		c.addEvent("0 BOOTSTRAP", "")
	}
	c.addEvent("1 BOOT", "")
	for {
		err := c.sim.runSession(ctx, c)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			c.retry = 0
		} else {
			c.retry++
		}
		if cfg.Once {
			return
		}

		// Follow-up actions requested by the ACS during the session.
		if dl := c.pendingDL; dl != nil {
			c.pendingDL = nil
			if !sleepCtx(ctx, dl.delay) {
				return
			}
			t := &transfer{key: dl.key, start: time.Now()}
			if cfg.FetchDownloads {
				if ferr := c.sim.fetch(ctx, dl); ferr != nil {
					t.faultCode, t.faultString = 9010, ferr.Error()
				}
			}
			t.complete = time.Now()
			c.transfer = t
			c.addEvent("7 TRANSFER COMPLETE", "")
			c.addEvent("M Download", dl.key)
			if t.faultCode == 0 && strings.HasPrefix(dl.fileType, "1") {
				if !sleepCtx(ctx, cfg.BootDelay) {
					return
				}
				c.bootTime = time.Now()
				c.addEvent("1 BOOT", "")
			}
			continue
		}
		if c.factoryReset {
			c.factoryReset = false
			c.rebootKey = nil
			if !sleepCtx(ctx, cfg.BootDelay) {
				return
			}
			c.initParams()
			c.bootTime = time.Now()
			c.events = []event{{"0 BOOTSTRAP", ""}, {"1 BOOT", ""}}
			continue
		}
		if c.rebootKey != nil {
			key := *c.rebootKey
			c.rebootKey = nil
			if !sleepCtx(ctx, cfg.BootDelay) {
				return
			}
			c.bootTime = time.Now()
			c.addEvent("1 BOOT", "")
			c.addEvent("M Reboot", key)
			continue
		}

		// Wait for the next reason to start a session.
		var wait time.Duration
		periodic := false
		switch {
		case err != nil:
			wait = retryBackoff(c.retry, c.interval)
		case c.periodicEnabled:
			wait, periodic = jitter(c.interval, cfg.Jitter), true
		default:
			wait = 365 * 24 * time.Hour
		}
		timer := time.NewTimer(wait)
		var schedC <-chan time.Time
		var sched *time.Timer
		if !c.scheduleAt.IsZero() {
			sched = time.NewTimer(time.Until(c.scheduleAt))
			schedC = sched.C
		}
		select {
		case <-ctx.Done():
		case <-timer.C:
			if periodic {
				c.addEvent("2 PERIODIC", "")
			}
		case <-c.crCh:
			c.addEvent("6 CONNECTION REQUEST", "")
		case <-schedC:
			c.addEvent("3 SCHEDULED", "")
			c.addEvent("M ScheduleInform", c.scheduleKey)
			c.scheduleAt = time.Time{}
		}
		timer.Stop()
		if sched != nil {
			sched.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// retryBackoff follows the TR-069 session retry policy (5s * 2^n, randomized), capped.
func retryBackoff(retry int, interval time.Duration) time.Duration {
	if retry < 1 {
		retry = 1
	}
	if retry > 8 {
		retry = 8
	}
	d := time.Duration(5<<uint(retry-1)) * time.Second
	if d > interval {
		d = interval
	}
	return d + time.Duration(rand.Int63n(int64(d)/2+1))
}

func jitter(d time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return d
	}
	delta := float64(d) * frac
	return time.Duration(float64(d) - delta + rand.Float64()*2*delta)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

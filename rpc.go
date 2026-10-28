package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// handleRPC answers one ACS->CPE RPC and returns the SOAP Body content.
func (c *CPE) handleRPC(method string, req *node) string {
	switch method {
	case "GetRPCMethods":
		return c.getRPCMethods()
	case "GetParameterValues":
		return c.getParameterValues(req)
	case "SetParameterValues":
		return c.setParameterValues(req)
	case "GetParameterNames":
		return c.getParameterNames(req)
	case "GetParameterAttributes":
		return c.getParameterAttributes(req)
	case "SetParameterAttributes":
		return "<cwmp:SetParameterAttributesResponse/>"
	case "AddObject":
		return c.addObject(req)
	case "DeleteObject":
		return c.deleteObject(req)
	case "Reboot":
		key := req.childText("CommandKey")
		c.rebootKey = &key
		return "<cwmp:RebootResponse/>"
	case "FactoryReset":
		c.factoryReset = true
		return "<cwmp:FactoryResetResponse/>"
	case "Download":
		return c.download(req)
	case "ScheduleInform":
		secs, _ := strconv.Atoi(req.childText("DelaySeconds"))
		c.scheduleAt = time.Now().Add(time.Duration(secs) * time.Second)
		c.scheduleKey = req.childText("CommandKey")
		return "<cwmp:ScheduleInformResponse/>"
	}
	return faultBody(9000, "")
}

func (c *CPE) getRPCMethods() string {
	methods := []string{"GetRPCMethods", "GetParameterNames", "GetParameterValues", "SetParameterValues",
		"GetParameterAttributes", "SetParameterAttributes", "AddObject", "DeleteObject", "Reboot",
		"FactoryReset", "Download", "ScheduleInform"}
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[%d]">`, len(methods))
	for _, m := range methods {
		b.WriteString("<string>" + m + "</string>")
	}
	b.WriteString("</MethodList></cwmp:GetRPCMethodsResponse>")
	return b.String()
}

// matchParams resolves a list of names/partial paths to parameter names,
// returning ok=false if any name is invalid.
func (c *CPE) matchParams(req *node) ([]string, bool) {
	var names []string
	if pl := req.child("ParameterNames"); pl != nil {
		for _, k := range pl.kids {
			names = append(names, strings.TrimSpace(k.text))
		}
	}
	var out []string
	var all []string
	var objs map[string]bool
	for _, n := range names {
		if n == "" || strings.HasSuffix(n, ".") {
			if all == nil {
				all, objs = c.sortedParams(), c.objects()
			}
			if n != "" && !objs[n] {
				return nil, false
			}
			for _, k := range all {
				if strings.HasPrefix(k, n) {
					out = append(out, k)
				}
			}
		} else if _, ok := c.params[n]; ok {
			out = append(out, n)
		} else {
			return nil, false
		}
	}
	return out, true
}

func (c *CPE) getParameterValues(req *node) string {
	names, ok := c.matchParams(req)
	if !ok {
		return faultBody(9005, "")
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:GetParameterValuesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[%d]">`, len(names))
	for _, n := range names {
		p := c.params[n]
		b.WriteString(pvs(n, p.typ, c.value(n, p)))
	}
	b.WriteString("</ParameterList></cwmp:GetParameterValuesResponse>")
	return b.String()
}

func validValue(typ, v string) bool {
	switch typ {
	case "xsd:unsignedInt":
		_, err := strconv.ParseUint(v, 10, 32)
		return err == nil
	case "xsd:int":
		_, err := strconv.ParseInt(v, 10, 32)
		return err == nil
	case "xsd:boolean":
		return v == "true" || v == "false" || v == "0" || v == "1"
	}
	return true
}

func (c *CPE) setParameterValues(req *node) string {
	type kv struct{ name, val string }
	var sets []kv
	var faults strings.Builder
	if pl := req.child("ParameterList"); pl != nil {
		for _, s := range pl.kids {
			name := s.childText("Name")
			val := ""
			if v := s.child("Value"); v != nil {
				val = v.text
			}
			code := 0
			p, ok := c.params[name]
			switch {
			case !ok:
				code = 9005
			case !p.writable:
				code = 9008
			case !validValue(p.typ, val):
				code = 9007
			}
			if code != 0 {
				fmt.Fprintf(&faults, "<SetParameterValuesFault><ParameterName>%s</ParameterName><FaultCode>%d</FaultCode><FaultString>%s</FaultString></SetParameterValuesFault>",
					esc(name), code, esc(faultMessages[code]))
				continue
			}
			sets = append(sets, kv{name, val})
		}
	}
	if faults.Len() > 0 {
		return faultBody(9003, faults.String())
	}
	for _, s := range sets {
		c.params[s.name].value = s.val
		switch {
		case strings.HasSuffix(s.name, "ManagementServer.PeriodicInformInterval"):
			if secs, _ := strconv.Atoi(s.val); secs > 0 {
				c.interval = time.Duration(secs) * time.Second
			}
		case strings.HasSuffix(s.name, "ManagementServer.PeriodicInformEnable"):
			c.periodicEnabled = s.val == "true" || s.val == "1"
		}
	}
	c.params[c.root+"ManagementServer.ParameterKey"].value = req.childText("ParameterKey")
	return "<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>"
}

func (c *CPE) getParameterNames(req *node) string {
	path := req.childText("ParameterPath")
	nl := req.childText("NextLevel")
	nextLevel := nl == "true" || nl == "1"
	objs := c.objects()
	type info struct {
		name     string
		writable bool
	}
	found := map[string]info{}

	switch {
	case path == "" && nextLevel:
		found[c.root] = info{c.root, false}
	case path == "" || strings.HasSuffix(path, "."):
		if path != "" && !objs[path] {
			return faultBody(9005, "")
		}
		if !nextLevel {
			for o := range objs {
				if strings.HasPrefix(o, path) {
					found[o] = info{o, false}
				}
			}
		}
		for name, p := range c.params {
			if !strings.HasPrefix(name, path) {
				continue
			}
			if !nextLevel {
				found[name] = info{name, p.writable}
				continue
			}
			rest := name[len(path):]
			if i := strings.IndexByte(rest, '.'); i >= 0 {
				o := path + rest[:i+1]
				found[o] = info{o, false}
			} else {
				found[name] = info{name, p.writable}
			}
		}
		if nextLevel {
			for o := range objs {
				if strings.HasPrefix(o, path) && o != path {
					rest := o[len(path):]
					if strings.IndexByte(rest, '.') == len(rest)-1 {
						found[o] = info{o, false}
					}
				}
			}
		}
	default:
		p, ok := c.params[path]
		if !ok {
			return faultBody(9005, "")
		}
		if nextLevel {
			return faultBody(9003, "")
		}
		found[path] = info{path, p.writable}
	}

	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:GetParameterNamesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterInfoStruct[%d]">`, len(names))
	for _, n := range names {
		w := "0"
		if found[n].writable {
			w = "1"
		}
		fmt.Fprintf(&b, "<ParameterInfoStruct><Name>%s</Name><Writable>%s</Writable></ParameterInfoStruct>", esc(n), w)
	}
	b.WriteString("</ParameterList></cwmp:GetParameterNamesResponse>")
	return b.String()
}

func (c *CPE) getParameterAttributes(req *node) string {
	names, ok := c.matchParams(req)
	if !ok {
		return faultBody(9005, "")
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<cwmp:GetParameterAttributesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterAttributeStruct[%d]">`, len(names))
	for _, n := range names {
		fmt.Fprintf(&b, `<ParameterAttributeStruct><Name>%s</Name><Notification>0</Notification><AccessList soap-enc:arrayType="xsd:string[0]"></AccessList></ParameterAttributeStruct>`, esc(n))
	}
	b.WriteString("</ParameterList></cwmp:GetParameterAttributesResponse>")
	return b.String()
}

func (c *CPE) addObject(req *node) string {
	obj := req.childText("ObjectName")
	if !strings.HasSuffix(obj, ".") || !c.objects()[obj] {
		return faultBody(9005, "")
	}
	// Find the highest existing instance and a template instance to clone.
	maxInst, tmpl := 0, ""
	for o := range c.objects() {
		if !strings.HasPrefix(o, obj) || o == obj {
			continue
		}
		rest := strings.TrimSuffix(o[len(obj):], ".")
		if n, err := strconv.Atoi(rest); err == nil {
			if n > maxInst {
				maxInst = n
			}
			if tmpl == "" || o < tmpl {
				tmpl = o
			}
		}
	}
	inst := maxInst + 1
	newObj := fmt.Sprintf("%s%d.", obj, inst)
	c.extraObjs[newObj] = true
	if tmpl != "" {
		for name, p := range c.params {
			if strings.HasPrefix(name, tmpl) {
				cp := *p
				c.params[newObj+name[len(tmpl):]] = &cp
			}
		}
	}
	c.params[c.root+"ManagementServer.ParameterKey"].value = req.childText("ParameterKey")
	return fmt.Sprintf("<cwmp:AddObjectResponse><InstanceNumber>%d</InstanceNumber><Status>0</Status></cwmp:AddObjectResponse>", inst)
}

func (c *CPE) deleteObject(req *node) string {
	obj := req.childText("ObjectName")
	if !strings.HasSuffix(obj, ".") || !c.objects()[obj] {
		return faultBody(9005, "")
	}
	for name := range c.params {
		if strings.HasPrefix(name, obj) {
			delete(c.params, name)
		}
	}
	for o := range c.extraObjs {
		if strings.HasPrefix(o, obj) {
			delete(c.extraObjs, o)
		}
	}
	c.params[c.root+"ManagementServer.ParameterKey"].value = req.childText("ParameterKey")
	return "<cwmp:DeleteObjectResponse><Status>0</Status></cwmp:DeleteObjectResponse>"
}

func (c *CPE) download(req *node) string {
	secs, _ := strconv.Atoi(req.childText("DelaySeconds"))
	c.pendingDL = &download{
		key:      req.childText("CommandKey"),
		fileType: req.childText("FileType"),
		url:      req.childText("URL"),
		user:     req.childText("Username"),
		pass:     req.childText("Password"),
		delay:    time.Duration(secs) * time.Second,
	}
	// Status 1 = download will complete later and be reported via TransferComplete.
	return "<cwmp:DownloadResponse><Status>1</Status><StartTime>0001-01-01T00:00:00Z</StartTime><CompleteTime>0001-01-01T00:00:00Z</CompleteTime></cwmp:DownloadResponse>"
}

package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const defaultCWMPNS = "urn:dslforum-org:cwmp-1-0"

// node is a minimal, namespace-agnostic XML tree used to read ACS messages.
type node struct {
	space, name string
	kids        []*node
	text        string
}

func parseXML(b []byte) (*node, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var root *node
	var stack []*node
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{space: t.Name.Space, name: t.Name.Local}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.kids = append(p.kids, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("empty XML document")
	}
	return root, nil
}

func (n *node) child(name string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

func (n *node) childText(name string) string {
	if c := n.child(name); c != nil {
		return strings.TrimSpace(c.text)
	}
	return ""
}

func (n *node) find(name string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
		if f := k.find(name); f != nil {
			return f
		}
	}
	return nil
}

// envelope is a parsed SOAP message from the ACS.
type envelope struct {
	ns     string // cwmp namespace used by the ACS
	id     string // cwmp:ID header
	method string // local name of the first Body element
	body   *node
}

func parseEnvelope(data []byte, fallbackNS string) (*envelope, error) {
	root, err := parseXML(data)
	if err != nil {
		return nil, fmt.Errorf("bad XML: %w", err)
	}
	if root.name != "Envelope" {
		return nil, fmt.Errorf("not a SOAP envelope (root <%s>)", root.name)
	}
	body := root.child("Body")
	if body == nil || len(body.kids) == 0 {
		return nil, errors.New("SOAP envelope has empty Body")
	}
	rpc := body.kids[0]
	env := &envelope{ns: fallbackNS, method: rpc.name, body: rpc}
	if rpc.name != "Fault" && rpc.space != "" {
		env.ns = rpc.space
	}
	if id := root.child("Header").find("ID"); id != nil {
		env.id = strings.TrimSpace(id.text)
	}
	return env, nil
}

func faultString(f *node) string {
	if cf := f.find("FaultCode"); cf != nil {
		return strings.TrimSpace(cf.text) + " " + f.find("Fault").childText("FaultString")
	}
	return f.childText("faultstring")
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func buildEnvelope(ns, id, body string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="`)
	b.WriteString(esc(ns))
	b.WriteString(`"><soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">`)
	b.WriteString(esc(id))
	b.WriteString(`</cwmp:ID></soap-env:Header><soap-env:Body>`)
	b.WriteString(body)
	b.WriteString(`</soap-env:Body></soap-env:Envelope>`)
	return b.Bytes()
}

var faultMessages = map[int]string{
	9000: "Method not supported",
	9002: "Internal error",
	9003: "Invalid arguments",
	9005: "Invalid parameter name",
	9006: "Invalid parameter type",
	9007: "Invalid parameter value",
	9008: "Attempt to set a non-writable parameter",
}

// faultBody builds a CWMP SOAP fault. extra is inserted inside cwmp:Fault
// (used for SetParameterValuesFault entries).
func faultBody(code int, extra string) string {
	return fmt.Sprintf(`<soap-env:Fault><faultcode>Client</faultcode><faultstring>CWMP fault</faultstring><detail><cwmp:Fault><FaultCode>%d</FaultCode><FaultString>%s</FaultString>%s</cwmp:Fault></detail></soap-env:Fault>`,
		code, esc(faultMessages[code]), extra)
}

package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

var errAuthRejected = errors.New("HTTP 401: credentials rejected")

// session is one CWMP session (Inform ... empty response) of one CPE.
type session struct {
	sim    *Sim
	c      *CPE
	client *http.Client
	tr     *http.Transport // non-nil if owned by this session

	scheme                               string // "", "basic", "digest"
	realm, nonce, opaque, qop, algorithm string
	nc                                   int
}

func (sim *Sim) newSession(c *CPE) *session {
	s := &session{sim: sim, c: c}
	tr := sim.sharedTr
	if tr == nil {
		tr = sim.makeTransport(c.idx)
		s.tr = tr
	}
	jar, _ := cookiejar.New(nil)
	s.client = &http.Client{Transport: tr, Jar: jar, Timeout: sim.cfg.Timeout}
	return s
}

func (s *session) close() {
	if s.tr != nil {
		s.tr.CloseIdleConnections()
	}
}

type phaseError struct {
	phase string
	err   error
}

func (e *phaseError) Error() string { return e.phase + ": " + e.err.Error() }

func perr(phase string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &phaseError{phase, err}
}

// run executes the full CWMP session.
func (s *session) run(ctx context.Context) error {
	c, st, cfg := s.c, s.sim.stats, s.sim.cfg

	t0 := time.Now()
	data, _, err := s.post(ctx, buildEnvelope(cfg.CWMPNS, c.nextID(), c.buildInform()))
	if err != nil {
		return perr("inform", err)
	}
	env, err := parseEnvelope(data, cfg.CWMPNS)
	if err != nil {
		return perr("inform", err)
	}
	switch env.method {
	case "InformResponse":
	case "Fault":
		return perr("inform", fmt.Errorf("ACS fault %s", faultString(env.body)))
	default:
		return perr("inform", fmt.Errorf("expected InformResponse, got %s", env.method))
	}
	st.informLat.add(time.Since(t0))
	st.informs.Add(1)
	c.events = nil

	if c.transfer != nil {
		data, _, err := s.post(ctx, buildEnvelope(env.ns, c.nextID(), c.buildTransferComplete()))
		if err != nil {
			return perr("transfercomplete", err)
		}
		tenv, err := parseEnvelope(data, env.ns)
		if err != nil {
			return perr("transfercomplete", err)
		}
		if tenv.method != "TransferCompleteResponse" {
			return perr("transfercomplete", fmt.Errorf("expected TransferCompleteResponse, got %s", tenv.method))
		}
		c.transfer = nil
	}

	var out []byte // empty POST tells the ACS we have nothing more to send
	for i := 0; i < 10000; i++ {
		data, code, err := s.post(ctx, out)
		if err != nil {
			return perr("rpc", err)
		}
		if code == http.StatusNoContent || len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		renv, err := parseEnvelope(data, env.ns)
		if err != nil {
			return perr("rpc", err)
		}
		st.countRPC(renv.method)
		if renv.method == "Fault" {
			out = nil
			continue
		}
		out = buildEnvelope(renv.ns, renv.id, c.handleRPC(renv.method, renv.body))
	}
	return perr("rpc", errors.New("too many RPCs in one session"))
}

// post sends one HTTP POST, handling Basic/Digest challenges.
func (s *session) post(ctx context.Context, body []byte) ([]byte, int, error) {
	cfg := s.sim.cfg
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ACSURL, bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
			req.Header.Set("SOAPAction", "")
		}
		req.Header.Set("User-Agent", "cpesim/1.0")
		hadAuth := s.scheme != ""
		s.authorize(req)
		if s.c.idx == cfg.Trace {
			log.Printf("[%s] >>> POST %s\n%s\n", s.c.serial, req.Header.Get("Authorization"), body)
		}
		s.sim.stats.bytesOut.Add(int64(len(body)))
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, 0, err
		}
		s.sim.stats.bytesIn.Add(int64(len(data)))
		if s.c.idx == cfg.Trace {
			log.Printf("[%s] <<< HTTP %d\n%s\n", s.c.serial, resp.StatusCode, data)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			s.sim.stats.authChallenges.Add(1)
			if err := s.handleChallenge(resp.Header.Values("WWW-Authenticate"), hadAuth); err != nil {
				return nil, resp.StatusCode, err
			}
			continue
		case http.StatusOK, http.StatusNoContent:
			return data, resp.StatusCode, nil
		}
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil, 0, errors.New("too many authentication attempts")
}

func (s *session) handleChallenge(hdrs []string, hadAuth bool) error {
	cfg := s.sim.cfg
	if cfg.User == "" {
		return errors.New("HTTP 401: ACS requires auth but no -user given")
	}
	var digest, basic string
	for _, h := range hdrs {
		l := strings.ToLower(strings.TrimSpace(h))
		if strings.HasPrefix(l, "digest ") {
			digest = strings.TrimSpace(h)[7:]
		} else if strings.HasPrefix(l, "basic") {
			basic = h
		}
	}
	switch {
	case digest != "":
		p := parseAuthParams(digest)
		if hadAuth && s.scheme == "digest" && !strings.EqualFold(p["stale"], "true") {
			return errAuthRejected
		}
		s.scheme, s.realm, s.nonce, s.opaque, s.algorithm = "digest", p["realm"], p["nonce"], p["opaque"], p["algorithm"]
		s.qop = ""
		for _, q := range strings.Split(p["qop"], ",") {
			if strings.TrimSpace(q) == "auth" {
				s.qop = "auth"
			}
		}
		s.nc = 0
		return nil
	case basic != "":
		if hadAuth && s.scheme == "basic" {
			return errAuthRejected
		}
		s.scheme = "basic"
		return nil
	}
	return errors.New("HTTP 401 without a supported WWW-Authenticate header")
}

func (s *session) authorize(req *http.Request) {
	cfg := s.sim.cfg
	switch s.scheme {
	case "basic":
		req.SetBasicAuth(cfg.User, cfg.Pass)
	case "digest":
		s.nc++
		nc := fmt.Sprintf("%08x", s.nc)
		cnonce := randHex(8)
		uri := req.URL.RequestURI()
		ha1 := md5hex(cfg.User + ":" + s.realm + ":" + cfg.Pass)
		if strings.EqualFold(s.algorithm, "MD5-sess") {
			ha1 = md5hex(ha1 + ":" + s.nonce + ":" + cnonce)
		}
		ha2 := md5hex(req.Method + ":" + uri)
		var h string
		if s.qop == "auth" {
			resp := md5hex(ha1 + ":" + s.nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
			h = fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
				cfg.User, s.realm, s.nonce, uri, nc, cnonce, resp)
		} else {
			resp := md5hex(ha1 + ":" + s.nonce + ":" + ha2)
			h = fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
				cfg.User, s.realm, s.nonce, uri, resp)
		}
		if s.algorithm != "" {
			h += ", algorithm=" + s.algorithm
		}
		if s.opaque != "" {
			h += fmt.Sprintf(`, opaque="%s"`, s.opaque)
		}
		req.Header.Set("Authorization", h)
	}
}

func parseAuthParams(s string) map[string]string {
	m := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ,\t")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return m
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		if strings.HasPrefix(s, `"`) {
			val, s = readQuoted(s[1:])
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val, s = strings.TrimSpace(s[:end]), s[end:]
		}
		m[key] = val
	}
}

func readQuoted(s string) (string, string) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), ""
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// fetch actually downloads a file requested by a Download RPC (-fetch-downloads).
func (sim *Sim) fetch(ctx context.Context, dl *download) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl.url, nil)
	if err != nil {
		return err
	}
	if dl.user != "" {
		req.SetBasicAuth(dl.user, dl.pass)
	}
	tr := sim.makeTransport(0)
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	sim.stats.bytesDownloaded.Add(n)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	return nil
}

// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package httpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"time"

	"codeberg.org/gruf/go-bytesize"
	"codeberg.org/gruf/go-cache/v3"
	errorsv2 "codeberg.org/gruf/go-errors/v2"
	"codeberg.org/gruf/go-iotools"
	"codeberg.org/gruf/go-kv"
	"github.com/superseriousbusiness/gotosocial/internal/gtscontext"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/log"
)

var (
	// ErrInvalidRequest is returned if a given HTTP request is invalid and cannot be performed.
	ErrInvalidRequest = errors.New("invalid http request")

	// ErrInvalidNetwork is returned if the request would not be performed over TCP
	ErrInvalidNetwork = errors.New("invalid network type")

	// ErrReservedAddr is returned if a dialed address resolves to an IP within a blocked or reserved net.
	ErrReservedAddr = errors.New("dial within blocked / reserved IP range")

	// ErrBodyTooLarge is returned when a received response body is above predefined limit (default 40MB).
	ErrBodyTooLarge = errors.New("body size too large")
)

// Config provides configuration details for setting up a new
// instance of httpclient.Client{}. Within are a subset of the
// configuration values passed to initialized http.Transport{}
// and http.Client{}, along with httpclient.Client{} specific.
type Config struct {
	// MaxOpenConnsPerHost limits the max number of open connections to a host.
	MaxOpenConnsPerHost int

	// MaxIdleConns: see http.Transport{}.MaxIdleConns.
	MaxIdleConns int

	// ReadBufferSize: see http.Transport{}.ReadBufferSize.
	ReadBufferSize int

	// WriteBufferSize: see http.Transport{}.WriteBufferSize.
	WriteBufferSize int

	// MaxBodySize determines the maximum fetchable body size.
	MaxBodySize int64

	// Timeout: see http.Client{}.Timeout.
	Timeout time.Duration

	// DisableCompression: see http.Transport{}.DisableCompression.
	DisableCompression bool

	// AllowRanges allows outgoing communications to given IP nets.
	AllowRanges []netip.Prefix

	// BlockRanges blocks outgoing communiciations to given IP nets.
	BlockRanges []netip.Prefix

	// TLSInsecureSkipVerify can be set to true to
	// skip validation of remote TLS certificates.
	//
	// THIS SHOULD BE USED FOR TESTING ONLY, IF YOU
	// TURN THIS ON WHILE RUNNING IN PRODUCTION YOU
	// ARE LEAVING YOUR SERVER WIDE OPEN TO ATTACKS!
	TLSInsecureSkipVerify bool
}

// Client wraps an underlying http.Client{} to provide the following:
//   - setting a maximum received request body size, returning error on
//     large content lengths, and using a limited reader in all other
//     cases to protect against forged / unknown content-lengths
//   - protection from server side request forgery (SSRF) by only dialing
//     out to known public IP prefixes, configurable with allows/blocks
//   - retry-backoff logic for error temporary HTTP error responses
//   - optional request signing
//   - request logging
type Client struct {
	client   http.Client
	badHosts cache.TTLCache[string, struct{}]
	bodyMax  int64
}

// New returns a new instance of Client initialized using configuration.
func New(cfg Config) *Client {
	var c Client

	d := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  &net.Resolver{},
	}

	if cfg.MaxOpenConnsPerHost <= 0 {
		// By default base this value on GOMAXPROCS.
		maxprocs := runtime.GOMAXPROCS(0)
		cfg.MaxOpenConnsPerHost = maxprocs * 20
	}

	if cfg.MaxIdleConns <= 0 {
		// By default base this value on MaxOpenConns.
		cfg.MaxIdleConns = cfg.MaxOpenConnsPerHost * 10
	}

	if cfg.MaxBodySize <= 0 {
		// By default set this to a reasonable 512MB.
		cfg.MaxBodySize = int64(512 * bytesize.MiB)
	}

	// Protect dialer with IP range sanitizer.
	d.Control = (&Sanitizer{
		Allow: cfg.AllowRanges,
		Block: cfg.BlockRanges,
	}).Sanitize

	// Prepare client fields.
	c.client.Timeout = cfg.Timeout
	c.bodyMax = cfg.MaxBodySize

	// Prepare TLS config for transport.
	tlsClientConfig := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify, //nolint:gosec
	}

	if tlsClientConfig.InsecureSkipVerify {
		// Warn against playing silly buggers.
		log.Warn(nil, "http-client.tls-insecure-skip-verify was set to TRUE. "+
			"*****THIS SHOULD BE USED FOR TESTING ONLY, IF YOU TURN THIS ON WHILE "+
			"RUNNING IN PRODUCTION YOU ARE LEAVING YOUR SERVER WIDE OPEN TO ATTACKS! "+
			"IF IN DOUBT, STOP YOUR SERVER *NOW* AND ADJUST YOUR CONFIGURATION!*****",
		)
	}

	// Set underlying HTTP client roundtripper.
	c.client.Transport = &signingtransport{http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		DialContext:           d.DialContext,
		TLSClientConfig:       tlsClientConfig,
		MaxIdleConns:          cfg.MaxIdleConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ReadBufferSize:        cfg.ReadBufferSize,
		WriteBufferSize:       cfg.WriteBufferSize,
		DisableCompression:    cfg.DisableCompression,
	}}

	// Initiate outgoing bad hosts lookup cache.
	c.badHosts = cache.NewTTL[string, struct{}](0, 1000, 0)
	c.badHosts.SetTTL(time.Hour, false)
	if !c.badHosts.Start(time.Minute) {
		log.Panic(nil, "failed to start transport controller cache")
	}

	return &c
}

// Do will essentially perform http.Client{}.Do() with retry-backoff functionality.
func (c *Client) Do(r *http.Request) (*http.Response, error) {
	return c.DoSigned(r, func(r *http.Request) error {
		return nil // no request signing
	})
}

// DoSigned will essentially perform http.Client{}.Do() with retry-backoff functionality and requesting signing..
func (c *Client) DoSigned(r *http.Request, sign SignFunc) (rsp *http.Response, err error) {
	const (
		// max no. attempts.
		maxRetries = 5

		// starting backoff duration.
		baseBackoff = 2 * time.Second
	)

	// First validate incoming request.
	if err := ValidateRequest(r); err != nil {
		return nil, err
	}

	// Get request hostname.
	host := r.URL.Hostname()

	// Check whether request should fast fail.
	fastFail := gtscontext.IsFastfail(r.Context())
	if !fastFail {
		// Check if recently reached max retries for this host
		// so we don't bother with a retry-backoff loop. The only
		// errors that are retried upon are server failure, TLS
		// and domain resolution type errors, so this cached result
		// indicates this server is likely having issues.
		fastFail = c.badHosts.Has(host)
		defer func() {
			if err != nil {
				// On error return mark as bad-host.
				c.badHosts.Set(host, struct{}{})
			}
		}()
	}

	// Start a log entry for this request
	l := log.WithContext(r.Context()).
		WithFields(kv.Fields{
			{"method", r.Method},
			{"url", r.URL.String()},
		}...)

	for i := 0; i < maxRetries; i++ {
		var backoff time.Duration

		l.Info("performing request")

		// Perform the request.
		rsp, err = c.do(r)
		if err == nil { //nolint:gocritic

			// TooManyRequest means we need to slow
			// down and retry our request. Codes over
			// 500 generally indicate temp. outages.
			if code := rsp.StatusCode; code < 500 &&
				code != http.StatusTooManyRequests {
				return rsp, nil
			}

			// Create loggable error from response status code.
			err = fmt.Errorf(`http response: %s`, rsp.Status)

			// Search for a provided "Retry-After" header value.
			if after := rsp.Header.Get("Retry-After"); after != "" {

				// Get current time.
				now := time.Now()

				if u, _ := strconv.ParseUint(after, 10, 32); u != 0 {
					// An integer number of backoff seconds was provided.
					backoff = time.Duration(u) * time.Second
				} else if at, _ := http.ParseTime(after); !at.Before(now) {
					// An HTTP formatted future date-time was provided.
					backoff = at.Sub(now)
				}

				// Don't let their provided backoff exceed our max.
				if max := baseBackoff * maxRetries; backoff > max {
					backoff = max
				}
			}

			// Close + unset rsp.
			_ = rsp.Body.Close()
			rsp = nil

		} else if errorsv2.IsV2(err,
			context.DeadlineExceeded,
			context.Canceled,
			ErrBodyTooLarge,
			ErrReservedAddr,
		) {
			// Non-retryable errors.
			return nil, err
		} else if errstr := err.Error(); // nocollapse
		strings.Contains(errstr, "stopped after 10 redirects") ||
			strings.Contains(errstr, "tls: ") ||
			strings.Contains(errstr, "x509: ") {
			// These error types aren't wrapped
			// so we have to check the error string.
			// All are unrecoverable!
			return nil, err
		} else if dnserr := (*net.DNSError)(nil); // nocollapse
		errors.As(err, &dnserr) && dnserr.IsNotFound {
			// DNS lookup failure, this domain does not exist
			return nil, gtserror.SetNotFound(err)
		}

		if fastFail {
			// on fast-fail, don't bother backoff/retry
			return nil, fmt.Errorf("%w (fast fail)", err)
		}

		if backoff == 0 {
			// No retry-after found, set our predefined
			// backoff according to a multiplier of 2^n.
			backoff = baseBackoff * 1 << (i + 1)
		}

		l.Errorf("backing off for %s after http request error: %v", backoff, err)

		select {
		// Request ctx cancelled
		case <-r.Context().Done():
			return nil, r.Context().Err()

		// Backoff for some time
		case <-time.After(backoff):
		}
	}

	// Set error return to trigger setting "bad host".
	err = errors.New("transport reached max retries")
	return
}

// do wraps http.Client{}.Do() to provide safely limited response bodies.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	// Perform the HTTP request.
	rsp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	// Seperate the body implementers.
	rbody := (io.Reader)(rsp.Body)
	cbody := (io.Closer)(rsp.Body)

	var limit int64

	if limit = rsp.ContentLength; limit < 0 {
		// If unknown, use max as reader limit.
		limit = c.bodyMax
	}

	// Don't trust them, limit body reads.
	rbody = io.LimitReader(rbody, limit)

	// Wrap closer to ensure entire body drained BEFORE close.
	cbody = iotools.CloserAfterCallback(cbody, func() {
		_, _ = discard.ReadFrom(rbody)
	})

	// Wrap body with limit.
	rsp.Body = &struct {
		io.Reader
		io.Closer
	}{rbody, cbody}

	// Check response body not too large.
	if rsp.ContentLength > c.bodyMax {
		_ = rsp.Body.Close()
		return nil, ErrBodyTooLarge
	}

	return rsp, nil
}

// cast discard writer to full interface it supports.
var discard = io.Discard.(interface { //nolint
	io.Writer
	io.StringWriter
	io.ReaderFrom
})

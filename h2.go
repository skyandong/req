// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package req

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"golang.org/x/net/http/httpguts"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var (
	http2VerboseLogs    bool
	http2logFrameWrites bool
	http2logFrameReads  bool
	http2inTests        bool
)

func init() {
	e := os.Getenv("GODEBUG")
	if strings.Contains(e, "http2debug=1") {
		http2VerboseLogs = true
	}
	if strings.Contains(e, "http2debug=2") {
		http2VerboseLogs = true
		http2logFrameWrites = true
		http2logFrameReads = true
	}
}

const (
	// ClientPreface is the string that must be sent by new
	// connections from clients.
	http2ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	// NextProtoTLS is the NPN/ALPN protocol negotiated during
	// HTTP/2's TLS setup.
	http2NextProtoTLS = "h2"

	// http://http2.github.io/http2-spec/#SettingValues
	http2initialHeaderTableSize = 4096

	http2initialWindowSize = 65535 // 6.9.2 Initial Flow Control Window Size
)

var (
	http2clientPreface = []byte(http2ClientPreface)
)

// Setting is a setting parameter: which setting it is, and its value.
type http2Setting struct {
	// ID is which setting is being set.
	// See http://http2.github.io/http2-spec/#SettingValues
	ID http2SettingID

	// Val is the value.
	Val uint32
}

func (s http2Setting) String() string {
	return fmt.Sprintf("[%v = %d]", s.ID, s.Val)
}

// Valid reports whether the setting is valid.
func (s http2Setting) Valid() error {
	// Limits and error codes from 6.5.2 Defined SETTINGS Parameters
	switch s.ID {
	case http2SettingEnablePush:
		if s.Val != 1 && s.Val != 0 {
			return http2ConnectionError(http2ErrCodeProtocol)
		}
	case http2SettingInitialWindowSize:
		if s.Val > 1<<31-1 {
			return http2ConnectionError(http2ErrCodeFlowControl)
		}
	case http2SettingMaxFrameSize:
		if s.Val < 16384 || s.Val > 1<<24-1 {
			return http2ConnectionError(http2ErrCodeProtocol)
		}
	}
	return nil
}

// A SettingID is an HTTP/2 setting as defined in
// http://http2.github.io/http2-spec/#iana-settings
type http2SettingID uint16

const (
	http2SettingHeaderTableSize      http2SettingID = 0x1
	http2SettingEnablePush           http2SettingID = 0x2
	http2SettingMaxConcurrentStreams http2SettingID = 0x3
	http2SettingInitialWindowSize    http2SettingID = 0x4
	http2SettingMaxFrameSize         http2SettingID = 0x5
	http2SettingMaxHeaderListSize    http2SettingID = 0x6
)

var http2settingName = map[http2SettingID]string{
	http2SettingHeaderTableSize:      "HEADER_TABLE_SIZE",
	http2SettingEnablePush:           "ENABLE_PUSH",
	http2SettingMaxConcurrentStreams: "MAX_CONCURRENT_STREAMS",
	http2SettingInitialWindowSize:    "INITIAL_WINDOW_SIZE",
	http2SettingMaxFrameSize:         "MAX_FRAME_SIZE",
	http2SettingMaxHeaderListSize:    "MAX_HEADER_LIST_SIZE",
}

func (s http2SettingID) String() string {
	if v, ok := http2settingName[s]; ok {
		return v
	}
	return fmt.Sprintf("UNKNOWN_SETTING_%d", uint16(s))
}

// validWireHeaderFieldName reports whether v is a valid header field
// name (key). See httpguts.ValidHeaderName for the base rules.
//
// Further, http2 says:
//   "Just as in HTTP/1.x, header field names are strings of ASCII
//   characters that are compared in a case-insensitive
//   fashion. However, header field names MUST be converted to
//   lowercase prior to their encoding in HTTP/2. "
func http2validWireHeaderFieldName(v string) bool {
	if len(v) == 0 {
		return false
	}
	for _, r := range v {
		if !httpguts.IsTokenRune(r) {
			return false
		}
		if 'A' <= r && r <= 'Z' {
			return false
		}
	}
	return true
}

func http2httpCodeString(code int) string {
	switch code {
	case 200:
		return "200"
	case 404:
		return "404"
	}
	return strconv.Itoa(code)
}

// bufWriterPoolBufferSize is the size of bufio.Writer's
// buffers created using bufWriterPool.
//
// TODO: pick a less arbitrary value? this is a bit under
// (3 x typical 1500 byte MTU) at least. Other than that,
// not much thought went into it.
const http2bufWriterPoolBufferSize = 4 << 10

var http2bufWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, http2bufWriterPoolBufferSize)
	},
}

func http2mustUint31(v int32) uint32 {
	if v < 0 || v > 2147483647 {
		panic("out of range")
	}
	return uint32(v)
}

// bodyAllowedForStatus reports whether a given response status code
// permits a body. See RFC 7230, section 3.3.
func http2bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}
	return true
}

type http2httpError struct {
	_       http2incomparable
	msg     string
	timeout bool
}

func (e *http2httpError) Error() string { return e.msg }

func (e *http2httpError) Timeout() bool { return e.timeout }

func (e *http2httpError) Temporary() bool { return true }

var http2errTimeout error = &http2httpError{msg: "http2: timeout awaiting response headers", timeout: true}

type http2connectionStater interface {
	ConnectionState() tls.ConnectionState
}

var http2sorterPool = sync.Pool{New: func() interface{} { return new(http2sorter) }}

type http2sorter struct {
	v []string // owned by sorter
}

func (s *http2sorter) Len() int { return len(s.v) }

func (s *http2sorter) Swap(i, j int) { s.v[i], s.v[j] = s.v[j], s.v[i] }

func (s *http2sorter) Less(i, j int) bool { return s.v[i] < s.v[j] }

// Keys returns the sorted keys of h.
//
// The returned slice is only valid until s used again or returned to
// its pool.
func (s *http2sorter) Keys(h http.Header) []string {
	keys := s.v[:0]
	for k := range h {
		keys = append(keys, k)
	}
	s.v = keys
	sort.Sort(s)
	return keys
}

func (s *http2sorter) SortStrings(ss []string) {
	// Our sorter works on s.v, which sorter owns, so
	// stash it away while we sort the user's buffer.
	save := s.v
	s.v = ss
	sort.Sort(s)
	s.v = save
}

// validPseudoPath reports whether v is a valid :path pseudo-header
// value. It must be either:
//
//     *) a non-empty string starting with '/'
//     *) the string '*', for OPTIONS requests.
//
// For now this is only used a quick check for deciding when to clean
// up Opaque URLs before sending requests from the Transport.
// See golang.org/issue/16847
//
// We used to enforce that the path also didn't start with "//", but
// Google's GFE accepts such paths and Chrome sends them, so ignore
// that part of the spec. See golang.org/issue/19103.
func http2validPseudoPath(v string) bool {
	return (len(v) > 0 && v[0] == '/') || v == "*"
}

// incomparable is a zero-width, non-comparable type. Adding it to a struct
// makes that struct also non-comparable, and generally doesn't add
// any size (as long as it's first).
type http2incomparable [0]func()

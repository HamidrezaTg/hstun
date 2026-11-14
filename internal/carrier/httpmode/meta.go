package httpmode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// The session id, sequence numbers and tag of a request used to travel in X-Hs-*
// headers, a plain tool signature for anything that can read the requests after
// TLS ends (a CDN, a reverse proxy's log). Now they travel in ordinary cookies
// whose names come from the secret http_path, so there is no constant name to
// look for:
//
//	Cookie: <a>=<session id hex>; <b>=<tag hex>; <c>=<seq>.<ack>.<max>
//
// The server answers a hello in the same style (Set-Cookie) when the hello carried
// a cookie, and in the old style when it did not, and understands both on every
// request, so old and new ends work together: a new client that gets an old-style
// answer to its hello simply continues with headers.
type metaNames struct{ id, tag, st string }

func namesFor(httpPath string) metaNames {
	h := sha256.Sum256([]byte("hstun-meta|" + httpPath))
	x := hex.EncodeToString(h[:])
	return metaNames{id: "s" + x[:8], tag: "t" + x[8:15], st: "v" + x[15:21]}
}

// reqMeta is what a request says about itself.
type reqMeta struct {
	id                string // session id or handshake id (hex)
	seq, ack, mx      uint64
	seqOK, ackOK, mxO bool
	tag               []byte
	cookie            bool // the request used the cookie style
}

func cookieVal(r *http.Request, name string) (string, bool) {
	c, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	return c.Value, true
}

// readMeta understands both styles.
func (n metaNames) readMeta(r *http.Request) reqMeta {
	var m reqMeta
	if id, ok := cookieVal(r, n.id); ok {
		m.cookie, m.id = true, id
		if t, ok := cookieVal(r, n.tag); ok {
			m.tag, _ = hex.DecodeString(t)
		}
		if st, ok := cookieVal(r, n.st); ok {
			f := strings.Split(st, ".")
			if len(f) == 3 {
				var e1, e2, e3 error
				m.seq, e1 = strconv.ParseUint(f[0], 10, 64)
				m.ack, e2 = strconv.ParseUint(f[1], 10, 64)
				m.mx, e3 = strconv.ParseUint(f[2], 10, 64)
				m.seqOK, m.ackOK, m.mxO = e1 == nil, e2 == nil, e3 == nil
			}
		}
		return m
	}
	m.id = r.Header.Get(hdrSid)
	if m.id == "" {
		m.id = r.Header.Get(hdrCid)
	}
	m.tag, _ = hex.DecodeString(r.Header.Get(hdrTag))
	m.seq, m.seqOK = hdrU64(r, hdrSeq)
	m.ack, m.ackOK = hdrU64(r, hdrAck)
	m.mx, m.mxO = hdrU64(r, hdrMax)
	return m
}

func hdrU64(r *http.Request, name string) (uint64, bool) {
	v, err := strconv.ParseUint(r.Header.Get(name), 10, 64)
	return v, err == nil
}

// writeReq sets the metadata on an outgoing request in the chosen style.
func (n metaNames) writeReq(h http.Header, cookie bool, id string, seq, ack uint64, mx int, tag []byte, hasSeq, hasAck bool, path string) {
	if !cookie {
		h.Set(hdrSid, id)
		if hasSeq {
			h.Set(hdrSeq, strconv.FormatUint(seq, 10))
		}
		if hasAck {
			h.Set(hdrAck, strconv.FormatUint(ack, 10))
		}
		if mx > 0 {
			h.Set(hdrMax, strconv.Itoa(mx))
		}
		h.Set(hdrTag, hex.EncodeToString(tag))
		return
	}
	v := n.id + "=" + id + "; " + n.tag + "=" + hex.EncodeToString(tag) + "; " +
		n.st + "=" + strconv.FormatUint(seq, 10) + "." + strconv.FormatUint(ack, 10) + "." + strconv.Itoa(mx)
	h.Set("Cookie", v)
}

// helloCookie is the marker a new client puts on its hello: a cookie that looks
// like a session id and tells the server which style to answer in.
func (n metaNames) helloCookie() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return n.id + "=" + hex.EncodeToString(b[:])
}

// setID writes a handshake or session id (the answer to a hello) in the style of the request.
func (n metaNames) setID(w http.ResponseWriter, cookie bool, id string) {
	if !cookie {
		w.Header().Set(hdrCid, id)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: n.id, Value: id, Path: "/", Secure: true, HttpOnly: true})
}

// responseID reads the id out of a hello response in either style.
func (n metaNames) responseID(h http.Header) (id string, cookie bool) {
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == n.id {
			return c.Value, true
		}
	}
	return h.Get(hdrCid), false
}

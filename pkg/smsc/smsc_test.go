package smsc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxGnu/gosmpp/data"
	"github.com/linxGnu/gosmpp/pdu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type esme struct {
	t    *testing.T
	conn net.Conn
}

func start(t *testing.T) (*Server, string, *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(ctx, map[string]string{"pee": "secret"})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ln.Close()) })
	go func() { assert.NoError(t, srv.Serve(ln)) }()
	ctl := httptest.NewServer(srv.Control())
	t.Cleanup(ctl.Close)
	return srv, ln.Addr().String(), ctl
}

func connect(t *testing.T, addr, password string) (*esme, data.CommandStatusType) {
	t.Helper()
	return connectAs(t, addr, password, pdu.NewBindTransceiver().(*pdu.BindRequest))
}

// connectAs binds with b, its credentials set here.
func connectAs(t *testing.T, addr, password string, b *pdu.BindRequest) (*esme, data.CommandStatusType) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	e := &esme{t: t, conn: c}
	b.SystemID, b.Password = "pee", password
	e.send(b)
	// Read the response raw: a refused bind is a header alone, which gosmpp's
	// decoder does not accept for a transceiver.
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	var h [16]byte
	_, err = io.ReadFull(c, h[:])
	require.NoError(t, err)
	n := binary.BigEndian.Uint32(h[0:4])
	_, err = io.ReadFull(c, make([]byte, n-16))
	require.NoError(t, err)
	return e, data.CommandStatusType(binary.BigEndian.Uint32(h[8:12]))
}

func (e *esme) send(p pdu.PDU) {
	e.t.Helper()
	buf := pdu.NewBuffer(nil)
	p.Marshal(buf)
	_, err := e.conn.Write(buf.Bytes())
	require.NoError(e.t, err)
}

func (e *esme) next() pdu.PDU {
	e.t.Helper()
	require.NoError(e.t, e.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	p, err := pdu.Parse(e.conn)
	require.NoError(e.t, err)
	return p
}

func submit(t *testing.T, to string, registered byte) *pdu.SubmitSM {
	t.Helper()
	s := pdu.NewSubmitSM().(*pdu.SubmitSM)
	var err error
	s.SourceAddr, err = pdu.NewAddressWithTonNpiAddr(1, 1, "15550001111")
	require.NoError(t, err)
	s.DestAddr, err = pdu.NewAddressWithTonNpiAddr(1, 1, to)
	require.NoError(t, err)
	require.NoError(t, s.Message.SetMessageWithEncoding("hello there", data.GSM7BIT))
	s.RegisteredDelivery = registered
	return s
}

func post(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, url, bytes.NewReader(b))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, resp.Body.Close()) })
	return resp
}

func TestBind(t *testing.T) {
	_, addr, _ := start(t)
	_, st := connect(t, addr, "secret")
	assert.Equal(t, data.ESME_ROK, st)
	_, st = connect(t, addr, "wrong")
	assert.Equal(t, data.ESME_RINVPASWD, st)
}

func TestSubmitAndReceipt(t *testing.T) {
	srv, addr, _ := start(t)
	srv.SetReceipts(Receipts{State: "UNDELIV"})
	e, _ := connect(t, addr, "secret")
	e.send(submit(t, "15550002222", 1))

	resp := e.next().(*pdu.SubmitSMResp)
	assert.Equal(t, data.ESME_ROK, resp.CommandStatus)
	require.NotEmpty(t, resp.MessageID)

	d := e.next().(*pdu.DeliverSM)
	assert.Equal(t, byte(0x04), d.EsmClass)
	assert.Equal(t, "15550002222", d.SourceAddr.Address())
	text, err := d.Message.GetMessage()
	require.NoError(t, err)
	assert.Contains(t, text, "id:"+resp.MessageID)
	assert.Contains(t, text, "stat:UNDELIV")
	assert.Equal(t, append([]byte(resp.MessageID), 0), d.OptionalParameters[pdu.TagReceiptedMessageID].Data)
	assert.Equal(t, []byte{5}, d.OptionalParameters[pdu.TagMessageStateOption].Data)

	recs := srv.Records("pee")
	require.Len(t, recs, 2)
	assert.Equal(t, "SUBMIT_SM", recs[1].Command)
	assert.Equal(t, "hello there", recs[1].Text)
}

func TestSubmitErrors(t *testing.T) {
	_, addr, ctl := start(t)
	resp := post(t, http.MethodPost, ctl.URL+"/mock/submit-errors", map[string]any{"statuses": []uint32{0x58}})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	e, _ := connect(t, addr, "secret")
	e.send(submit(t, "15550002222", 0))
	assert.Equal(t, data.ESME_RTHROTTLED, e.next().GetHeader().CommandStatus)
	e.send(submit(t, "15550002222", 0))
	assert.Equal(t, data.ESME_ROK, e.next().GetHeader().CommandStatus)
}

func TestMOMultipart(t *testing.T) {
	_, addr, ctl := start(t)
	e, _ := connect(t, addr, "secret")
	text := string(bytes.Repeat([]byte("a"), 200))

	done := make(chan *http.Response, 1)
	go func() {
		done <- post(t, http.MethodPost, ctl.URL+"/mock/mo", map[string]string{"system_id": "pee", "source": "15550002222", "destination": "15550001111", "text": text})
	}()
	var got string
	for i := range 2 {
		d := e.next().(*pdu.DeliverSM)
		assert.Equal(t, byte(0x40), d.EsmClass)
		total, part, _, ok := d.Message.UDH().GetConcatInfo()
		require.True(t, ok)
		assert.Equal(t, byte(2), total)
		assert.Equal(t, byte(i+1), part)
		m, err := d.Message.GetMessage()
		require.NoError(t, err)
		got += m
		e.send(d.GetResponse())
	}
	assert.Equal(t, text, got)
	resp := <-done
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDrop(t *testing.T) {
	_, addr, ctl := start(t)
	e, _ := connect(t, addr, "secret")
	resp := post(t, http.MethodPost, ctl.URL+"/mock/drop", map[string]string{"system_id": "pee"})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, e.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := pdu.Parse(e.conn)
	assert.Error(t, err)
}

func TestSplit(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		parts int
		enc   data.Encoding
	}{
		{"gsm7 single", strings.Repeat("a", 160), 1, data.GSM7BIT},
		{"gsm7 two", strings.Repeat("a", 161), 2, data.GSM7BIT},
		{"escapes count twice", strings.Repeat("|", 81), 2, data.GSM7BIT},
		{"ucs2 single", strings.Repeat("é€你", 23), 1, data.UCS2},
		{"ucs2 two", strings.Repeat("你", 71), 2, data.UCS2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc, parts := split(tc.text)
			assert.Equal(t, tc.enc, enc)
			assert.Len(t, parts, tc.parts)
			assert.Equal(t, tc.text, strings.Join(parts, ""))
		})
	}
}

func TestMORequiresSystemID(t *testing.T) {
	srv, _, _ := start(t)
	_, err := srv.MO(context.Background(), "", "1", "2", "hi")
	assert.Error(t, err)
}

func TestHoldResponses(t *testing.T) {
	srv, addr, ctl := start(t)
	srv.SetReceipts(Receipts{None: true})
	e, _ := connect(t, addr, "secret")

	resp := post(t, http.MethodPut, ctl.URL+"/mock/responses", map[string]bool{"hold": true})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	e.send(submit(t, "15550002222", 0))
	require.Eventually(t, func() bool { return srv.Held() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, e.conn.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, err := pdu.Parse(e.conn)
	require.Error(t, err, "nothing answered while held")

	resp = post(t, http.MethodPost, ctl.URL+"/mock/responses/release", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, data.ESME_ROK, e.next().(*pdu.SubmitSMResp).CommandStatus)
	assert.Zero(t, srv.Held())

	e.send(submit(t, "15550002222", 0))
	assert.Equal(t, data.ESME_ROK, e.next().GetHeader().CommandStatus, "answered at once after the release")
}

// A receipt with no bind to take it waits for the next bind, as an SMSC
// stores it.
func TestReceiptWaitsForTheNextBind(t *testing.T) {
	srv, addr, _ := start(t)
	// A transmitter bind takes no receipts, so none has a bind to go to.
	e, _ := connectAs(t, addr, "secret", pdu.NewBindTransmitter().(*pdu.BindRequest))
	e.send(submit(t, "15550002222", 1))
	id := e.next().(*pdu.SubmitSMResp).MessageID

	// The receipt waits, then goes to the next receiving bind.
	require.Eventually(t, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.pending["pee"]) == 1
	}, 5*time.Second, 10*time.Millisecond)
	again, _ := connect(t, addr, "secret")
	d := again.next().(*pdu.DeliverSM)
	text, err := d.Message.GetMessage()
	require.NoError(t, err)
	assert.Contains(t, text, "id:"+id)
}

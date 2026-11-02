package smsc

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
	c, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	e := &esme{t: t, conn: c}
	b := pdu.NewBindTransceiver().(*pdu.BindRequest)
	b.SystemID, b.Password = "pee", password
	e.send(b)
	return e, e.next().GetHeader().CommandStatus
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

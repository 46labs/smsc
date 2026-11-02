// Package smsc is an SMPP 3.4 SMSC mock: it takes binds, answers submits,
// sends delivery receipts and MO, and records every PDU for tests.
package smsc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/linxGnu/gosmpp/data"
	"github.com/linxGnu/gosmpp/pdu"
)

// Receipts is how the mock answers a submit that asks for a receipt.
type Receipts struct {
	// State is the Appendix B stat, e.g. DELIVRD, UNDELIV, EXPIRED, REJECTD.
	State string `json:"state"`
	// Delay before the receipt is sent.
	Delay time.Duration `json:"delay"`
	// None sends no receipt.
	None bool `json:"none"`
}

// Record is one PDU the mock received.
type Record struct {
	SystemID           string    `json:"system_id"`
	Command            string    `json:"command"`
	Seq                int32     `json:"seq"`
	Status             uint32    `json:"status"`
	Source             string    `json:"source,omitempty"`
	Destination        string    `json:"destination,omitempty"`
	Text               string    `json:"text,omitempty"`
	EsmClass           byte      `json:"esm_class"`
	DataCoding         byte      `json:"data_coding"`
	RegisteredDelivery byte      `json:"registered_delivery"`
	MessageID          string    `json:"message_id,omitempty"`
	At                 time.Time `json:"at"`
}

// Server is the mock. Accounts maps system_id to password.
type Server struct {
	accounts map[string]string

	mu       sync.Mutex
	sessions map[*session]struct{}
	records  []Record
	errors   []data.CommandStatusType
	receipts Receipts
	nextID   uint64
	nextRef  byte
	ctx      context.Context
}

// New returns a mock that accepts the given accounts.
func New(ctx context.Context, accounts map[string]string) *Server {
	return &Server{
		accounts: accounts,
		sessions: map[*session]struct{}{},
		receipts: Receipts{State: "DELIVRD", Delay: 100 * time.Millisecond},
		ctx:      ctx,
	}
}

// Serve accepts binds on ln until it closes.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(c)
	}
}

type session struct {
	conn     net.Conn
	systemID string
	receives bool // a receiver or transceiver bind
	done     chan struct{}
	mu       sync.Mutex // writes and acks
	acks     map[int32]chan data.CommandStatusType
}

func (ss *session) write(p pdu.PDU) error {
	buf := pdu.NewBuffer(make([]byte, 0, 64))
	p.Marshal(buf)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if err := ss.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err := ss.conn.Write(buf.Bytes())
	return err
}

func (s *Server) handle(c net.Conn) {
	ss := &session{conn: c, done: make(chan struct{}), acks: map[int32]chan data.CommandStatusType{}}
	defer func() {
		close(ss.done)
		s.mu.Lock()
		delete(s.sessions, ss)
		s.mu.Unlock()
		if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("close", "error", err)
		}
	}()
	for {
		p, err := pdu.Parse(c)
		if err != nil {
			slog.Debug("read", "system_id", ss.systemID, "error", err)
			return
		}
		if !s.dispatch(ss, p) {
			return
		}
	}
}

// dispatch answers one PDU; false ends the connection.
func (s *Server) dispatch(ss *session, p pdu.PDU) bool {
	if ss.systemID == "" {
		b, ok := p.(*pdu.BindRequest)
		if !ok {
			s.reply(ss, nack(p, data.ESME_RINVBNDSTS))
			return false
		}
		return s.bind(ss, b)
	}
	s.record(ss, p)
	switch v := p.(type) {
	case *pdu.SubmitSM:
		s.submit(ss, v)
	case *pdu.EnquireLink:
		s.reply(ss, v.GetResponse())
	case *pdu.Unbind:
		s.reply(ss, v.GetResponse())
		return false
	case *pdu.DeliverSMResp, *pdu.DataSMResp, *pdu.GenericNack:
		ss.mu.Lock()
		ack, ok := ss.acks[p.GetSequenceNumber()]
		delete(ss.acks, p.GetSequenceNumber())
		ss.mu.Unlock()
		if ok {
			ack <- p.GetHeader().CommandStatus
		}
	case *pdu.EnquireLinkResp, *pdu.UnbindResp:
	default:
		s.reply(ss, nack(p, data.ESME_RINVCMDID))
	}
	return true
}

func (s *Server) bind(ss *session, b *pdu.BindRequest) bool {
	resp := pdu.NewBindResp(*b)
	pw, ok := s.accounts[b.SystemID]
	switch {
	case !ok:
		resp.CommandStatus = data.ESME_RINVSYSID
	case pw != b.Password:
		resp.CommandStatus = data.ESME_RINVPASWD
	}
	s.record(&session{systemID: b.SystemID}, b)
	if resp.CommandStatus != data.ESME_ROK {
		// An error response is its header alone.
		if err := ss.writeHeader(resp.CommandID, resp.CommandStatus, b.SequenceNumber); err != nil {
			slog.Debug("bind refuse", "system_id", b.SystemID, "error", err)
		}
		return false
	}
	resp.SystemID = "smsc-mock"
	ss.systemID = b.SystemID
	ss.receives = b.BindingType != pdu.Transmitter
	s.mu.Lock()
	s.sessions[ss] = struct{}{}
	s.mu.Unlock()
	return s.reply(ss, resp)
}

// writeHeader writes a PDU that is only a header.
func (ss *session) writeHeader(id data.CommandIDType, status data.CommandStatusType, seq int32) error {
	var h [16]byte
	binary.BigEndian.PutUint32(h[0:], 16)
	binary.BigEndian.PutUint32(h[4:], uint32(id))
	binary.BigEndian.PutUint32(h[8:], uint32(status))
	binary.BigEndian.PutUint32(h[12:], uint32(seq))
	ss.mu.Lock()
	defer ss.mu.Unlock()
	_, err := ss.conn.Write(h[:])
	return err
}

func (s *Server) submit(ss *session, v *pdu.SubmitSM) {
	resp := pdu.NewSubmitSMRespFromReq(v).(*pdu.SubmitSMResp)
	s.mu.Lock()
	if len(s.errors) > 0 {
		resp.CommandStatus = s.errors[0]
		s.errors = s.errors[1:]
	} else {
		s.nextID++
		resp.MessageID = fmt.Sprintf("%010x", s.nextID)
	}
	receipts := s.receipts
	s.mu.Unlock()
	if !s.reply(ss, resp) || resp.CommandStatus != data.ESME_ROK {
		return
	}
	if v.RegisteredDelivery&0x03 == 0 || receipts.None {
		return
	}
	if v.RegisteredDelivery&0x03 == 2 && receipts.State == "DELIVRD" {
		return // failure receipts only
	}
	text := messageText(&v.Message)
	go func() {
		select {
		case <-time.After(receipts.Delay):
		case <-s.ctx.Done():
			return
		}
		to := ss
		if !ss.receives {
			to = s.receiver(ss.systemID)
		}
		if to == nil {
			slog.Debug("receipt: no receiving bind", "system_id", ss.systemID, "message_id", resp.MessageID)
			return
		}
		r, err := receipt(v, resp.MessageID, receipts.State, text, time.Now())
		if err == nil {
			err = to.write(r)
		}
		if err != nil {
			slog.Debug("receipt", "message_id", resp.MessageID, "error", err)
		}
	}()
}

// messageStates maps Appendix B stats to the message_state TLV value.
var messageStates = map[string]byte{
	"ENROUTE": 1, "DELIVRD": 2, "EXPIRED": 3, "DELETED": 4,
	"UNDELIV": 5, "ACCEPTD": 6, "UNKNOWN": 7, "REJECTD": 8,
}

// receipt builds an SMSC delivery receipt for a submit: Appendix B text and
// the receipted_message_id and message_state TLVs.
func receipt(v *pdu.SubmitSM, id, state, text string, now time.Time) (*pdu.DeliverSM, error) {
	d := pdu.NewDeliverSM().(*pdu.DeliverSM)
	d.SourceAddr = v.DestAddr
	d.DestAddr = v.SourceAddr
	d.EsmClass = 0x04
	dlvrd := "000"
	if state == "DELIVRD" {
		dlvrd = "001"
	}
	excerpt := []rune{}
	for _, c := range text {
		if len(excerpt) == 20 {
			break
		}
		if !strings.ContainsRune(gsm7Chars, c) {
			c = '?'
		}
		excerpt = append(excerpt, c)
	}
	text = string(excerpt)
	stamp := now.UTC().Format("0601021504")
	body := fmt.Sprintf("id:%s sub:001 dlvrd:%s submit date:%s done date:%s stat:%s err:000 text:%s", id, dlvrd, stamp, stamp, state, text)
	if err := d.Message.SetMessageWithEncoding(body, data.GSM7BIT); err != nil {
		return nil, fmt.Errorf("receipt text: %w", err)
	}
	d.RegisterOptionalParam(pdu.Field{Tag: pdu.TagReceiptedMessageID, Data: append([]byte(id), 0)})
	d.RegisterOptionalParam(pdu.Field{Tag: pdu.TagMessageStateOption, Data: []byte{messageStates[state]}})
	return d, nil
}

// messageText is a short message's text, empty when its coding is one the
// codec does not know.
func messageText(m *pdu.ShortMessage) string {
	if m.Encoding() == nil {
		return ""
	}
	t, err := m.GetMessage()
	if err != nil {
		return ""
	}
	return t
}

func (s *Server) reply(ss *session, p pdu.PDU) bool {
	if err := ss.write(p); err != nil {
		slog.Debug("write", "system_id", ss.systemID, "error", err)
		return false
	}
	return true
}

func nack(p pdu.PDU, status data.CommandStatusType) pdu.PDU {
	n := pdu.NewGenericNack()
	n.SetSequenceNumber(p.GetSequenceNumber())
	n.(*pdu.GenericNack).CommandStatus = status
	return n
}

func (s *Server) record(ss *session, p pdu.PDU) {
	h := p.GetHeader()
	r := Record{SystemID: ss.systemID, Command: h.CommandID.String(), Seq: h.SequenceNumber, Status: uint32(h.CommandStatus), At: time.Now()}
	if v, ok := p.(*pdu.SubmitSM); ok {
		r.Source, r.Destination = v.SourceAddr.Address(), v.DestAddr.Address()
		r.EsmClass, r.RegisteredDelivery = v.EsmClass, v.RegisteredDelivery
		if enc := v.Message.Encoding(); enc != nil {
			r.DataCoding = enc.DataCoding()
		}
		r.Text = messageText(&v.Message)
	}
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
}

// Records returns the PDUs received from systemID, or from every bind when
// it is empty.
func (s *Server) Records(systemID string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Record{}
	for _, r := range s.records {
		if systemID == "" || r.SystemID == systemID {
			out = append(out, r)
		}
	}
	return out
}

// FailSubmits answers the next submits with statuses, one each, in order.
func (s *Server) FailSubmits(statuses []data.CommandStatusType) {
	s.mu.Lock()
	s.errors = append(s.errors, statuses...)
	s.mu.Unlock()
}

// SetReceipts sets how later submits are receipted.
func (s *Server) SetReceipts(r Receipts) {
	s.mu.Lock()
	s.receipts = r
	s.mu.Unlock()
}

// Drop closes every bind of systemID, or every bind when it is empty, and
// returns how many it closed.
func (s *Server) Drop(systemID string) int {
	n := 0
	for _, ss := range s.binds(systemID) {
		if err := ss.conn.Close(); err != nil {
			slog.Debug("drop", "error", err)
		}
		n++
	}
	return n
}

// Reset forgets the records, queued errors and receipt settings.
func (s *Server) Reset() {
	s.mu.Lock()
	s.records, s.errors = nil, nil
	s.receipts = Receipts{State: "DELIVRD", Delay: 100 * time.Millisecond}
	s.mu.Unlock()
}

// receiver is a bind of systemID that can take deliver_sm.
func (s *Server) receiver(systemID string) *session {
	for _, ss := range s.binds(systemID) {
		if ss.receives {
			return ss
		}
	}
	return nil
}

func (s *Server) binds(systemID string) []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*session
	for ss := range s.sessions {
		if systemID == "" || ss.systemID == systemID {
			out = append(out, ss)
		}
	}
	return out
}

// MO sends a mobile-originated message to a receiving bind of systemID,
// split with an 8-bit UDH when it does not fit one part, and returns the
// status each part was answered with.
func (s *Server) MO(ctx context.Context, systemID, source, destination, text string) ([]uint32, error) {
	if systemID == "" {
		return nil, errors.New("system_id is required")
	}
	ss := s.receiver(systemID)
	if ss == nil {
		return nil, fmt.Errorf("no receiving bind for %q", systemID)
	}
	enc, parts := split(text)
	s.mu.Lock()
	s.nextRef++
	ref := s.nextRef
	s.mu.Unlock()
	var statuses []uint32
	for i, part := range parts {
		d := pdu.NewDeliverSM().(*pdu.DeliverSM)
		var err error
		if d.SourceAddr, err = pdu.NewAddressWithTonNpiAddr(1, 1, source); err != nil {
			return nil, err
		}
		if d.DestAddr, err = pdu.NewAddressWithTonNpiAddr(1, 1, destination); err != nil {
			return nil, err
		}
		if d.Message, err = pdu.NewShortMessageWithEncoding(part, enc); err != nil {
			return nil, err
		}
		if len(parts) > 1 {
			d.EsmClass = 0x40
			d.Message.SetUDH(pdu.UDH{pdu.NewIEConcatMessage(byte(len(parts)), byte(i+1), ref)})
		}
		st, err := ss.exchange(ctx, d)
		if err != nil {
			return statuses, err
		}
		statuses = append(statuses, uint32(st))
	}
	return statuses, nil
}

// exchange writes d and waits for its response.
func (ss *session) exchange(ctx context.Context, d *pdu.DeliverSM) (data.CommandStatusType, error) {
	ack := make(chan data.CommandStatusType, 1)
	seq := d.SequenceNumber
	ss.mu.Lock()
	ss.acks[seq] = ack
	ss.mu.Unlock()
	defer func() {
		ss.mu.Lock()
		delete(ss.acks, seq)
		ss.mu.Unlock()
	}()
	if err := ss.write(d); err != nil {
		return 0, err
	}
	select {
	case st := <-ack:
		return st, nil
	case <-ss.done:
		return 0, errors.New("bind closed before the response")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// split cuts text into SMS parts by encoded length: one part of 160 GSM-7
// septets or 70 UCS-2 units, else parts of 153 or 67, never splitting a
// GSM-7 escape pair.
func split(text string) (data.Encoding, []string) {
	enc, single, multi := data.Encoding(data.GSM7BIT), 160, 153
	if !gsm7(text) {
		enc, single, multi = data.UCS2, 70, 67
	}
	size := func(r rune) int {
		switch {
		case enc == data.UCS2 && r > 0xFFFF:
			return 2
		case enc == data.GSM7BIT && strings.ContainsRune(gsm7Ext, r):
			return 2
		}
		return 1
	}
	total := 0
	for _, r := range text {
		total += size(r)
	}
	if total <= single {
		return enc, []string{text}
	}
	var parts []string
	var cur []rune
	n := 0
	for _, r := range text {
		if n+size(r) > multi {
			parts = append(parts, string(cur))
			cur, n = nil, 0
		}
		cur = append(cur, r)
		n += size(r)
	}
	return enc, append(parts, string(cur))
}

// gsm7 reports whether text fits the GSM 03.38 alphabet.
func gsm7(text string) bool {
	for _, r := range text {
		if !strings.ContainsRune(gsm7Chars, r) {
			return false
		}
	}
	return true
}

// gsm7Ext are the GSM 03.38 extension characters, two septets each.
const gsm7Ext = "^{}\\[~]|€"

const gsm7Chars = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà^{}\\[~]|€"

package usage

import (
	"errors"
	"log"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

var ErrReceiptUnbound = errors.New("receipt_unbound")

func ReceiptCode(err error) string {
	if errors.Is(err, ErrReceiptUnbound) {
		return "receipt_unbound"
	}
	return mediacontracts.ReceiptWireError(err)
}

func (c *Connection) ReceiptBound() bool { return c.info.ReceiptKey != "" }

func (c *Connection) Ended() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closedAt.IsZero() || c.draining
}

func (c *Connection) startReceipts() {
	if c.receipts == nil && c.info.ReceiptKey != "" && c.info.TokenNonce != nil {
		config := c.reporter.config
		c.receipts = mediacontracts.NewReceiptConnection(mediacontracts.ReceiptBinding{Session: config.SessionID,
			Node: config.NodeID, Connection: c.id, Nonce: *c.info.TokenNonce}, c.info.ReceiptKey, c.reporter.MonoMS())
	}
}

func (c *Connection) Receipt(value string) error {
	_, _, err := c.receipt(value, true)
	return err
}

func (c *Connection) ReceiptTime(value string) (receipt mediacontracts.Receipt, taken bool, err error) {
	return c.receipt(value, false)
}

func (c *Connection) receipt(value string, compareBytes bool) (mediacontracts.Receipt, bool, error) {
	if !c.ReceiptBound() {
		return mediacontracts.Receipt{}, false, ErrReceiptUnbound
	}
	receipt, err := mediacontracts.ParseReceipt(value)
	if err != nil {
		return mediacontracts.Receipt{}, false, err
	}
	at := c.reporter.NowMS()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.receipts
	if state == nil || !c.closedAt.IsZero() || c.draining {
		return mediacontracts.Receipt{}, false, mediacontracts.ErrReceiptConnection
	}
	bytes := receipt.MediaBytes
	if compareBytes {
		bytes = c.media.Load()
	}
	duplicate, err := state.Accept(value, c.reporter.MonoMS(), c.presence.total(at), bytes)
	if err != nil || duplicate {
		return receipt, false, err
	}
	c.latest = &mediacontracts.UsageReceipt{Value: value, ReceivedMS: at}
	return receipt, true, nil
}

func (c *Connection) LastReceipt() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.receipts == nil {
		return 0
	}
	return c.receipts.Sequence()
}

func (c *Connection) ReceiptRefusal() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closedAt.IsZero() || c.draining || c.receipts == nil {
		return ""
	}
	return c.receipts.Refusal(c.reporter.MonoMS())
}

func (c *Connection) NoteReceiptRefusal(reason string) {
	c.mu.Lock()
	noted := c.receiptNoted
	c.receiptNoted = true
	c.mu.Unlock()
	if !noted {
		c.reporter.receiptRefusals.Add(1)
		log.Printf("media %s: connection %s: %s (not enforced)", c.reporter.config.NodeID, c.id, reason)
	}
}

func (r *Reporter) ReceiptRefusals() int64 { return r.receiptRefusals.Load() }

func (c *Connection) takeReceipt(end int64) *mediacontracts.UsageReceipt {
	latest := c.latest
	if latest == nil || latest.ReceivedMS > end {
		return nil
	}
	c.latest = nil
	return latest
}

func EnforcesReceipts(settings *mediacontracts.UsageSettings, transport string) bool {
	if settings == nil {
		return false
	}
	for _, enforced := range settings.Receipts.Enforce {
		if enforced == transport {
			return true
		}
	}
	return false
}

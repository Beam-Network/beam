package mediacontracts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"
	"sync"
)

const (
	SFrameScheme           = "beam-sframe-v1"
	SFrameCipherSuite      = 0x0004
	SFrameKeyBytes         = 16
	SFrameNonceBytes       = 12
	SFrameTagBytes         = 16
	SFrameHashBytes        = 32
	SFrameEpochBits        = 8
	SFrameSenderIndexBits  = 16
	SFrameInstanceBits     = 24
	SFrameSenderSlotBits   = 16
	SFrameReplayWindow     = 1024
	SFrameMLSExporterLabel = "SFrame 1.0 Base Key"
	SFrameCustomerKeySalt  = "beam-sframe-v1 customer key"
)

var (
	ErrSFrameHeader         = errors.New("sframe: invalid header")
	ErrSFrameAuthentication = errors.New("sframe: authentication failed")
	ErrSFrameKID            = errors.New("sframe: invalid KID fields")
	ErrSFrameCodec          = errors.New("sframe: codec not allowed with beam-sframe-v1")
	ErrSFrameFrame          = errors.New("sframe: frame shorter than its clear prefix")
	ErrSFrameNoKey          = errors.New("sframe: no key for KID")
	ErrSFrameReplay         = errors.New("sframe: replayed or too old counter")
	ErrSFrameExhausted      = errors.New("sframe: counter exhausted")
	ErrSFrameLength         = errors.New("sframe: derived length out of range")
	ErrSFrameKIDLimit       = errors.New("sframe: too many KIDs held by the receiver")
)

func EncodeSFrameHeader(kid, ctr uint64) []byte {
	header := []byte{0}
	var kidBytes, ctrBytes []byte
	if kid < 8 {
		header[0] |= byte(kid) << 4
	} else {
		kidBytes = minimalBigEndian(kid)
		header[0] |= 0x80 | byte(len(kidBytes)-1)<<4
	}
	if ctr < 8 {
		header[0] |= byte(ctr)
	} else {
		ctrBytes = minimalBigEndian(ctr)
		header[0] |= 0x08 | byte(len(ctrBytes)-1)
	}
	header = append(header, kidBytes...)
	return append(header, ctrBytes...)
}

func minimalBigEndian(value uint64) []byte {
	length := (bits.Len64(value) + 7) / 8
	if length == 0 {
		length = 1
	}
	out := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		out[i] = byte(value)
		value >>= 8
	}
	return out
}

func DecodeSFrameHeader(data []byte) (kid, ctr uint64, length int, err error) {
	if len(data) < 1 {
		return 0, 0, 0, ErrSFrameHeader
	}
	config := data[0]
	offset := 1
	readField := func(extended bool, value byte) (uint64, error) {
		if !extended {
			return uint64(value), nil
		}
		size := int(value) + 1
		if len(data) < offset+size {
			return 0, ErrSFrameHeader
		}
		var result uint64
		for _, b := range data[offset : offset+size] {
			result = result<<8 | uint64(b)
		}
		if result < 8 || len(minimalBigEndian(result)) != size {
			return 0, ErrSFrameHeader
		}
		offset += size
		return result, nil
	}
	if kid, err = readField(config&0x80 != 0, (config>>4)&0x07); err != nil {
		return 0, 0, 0, err
	}
	if ctr, err = readField(config&0x08 != 0, config&0x07); err != nil {
		return 0, 0, 0, err
	}
	return kid, ctr, offset, nil
}

func hkdfExtract(salt, ikm []byte) []byte {
	if salt == nil {
		salt = []byte{}
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	if length < 0 || length > 255*sha256.Size {
		panic("hkdf: length out of range")
	}
	var out, previous []byte
	for counter := byte(1); len(out) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(previous)
		mac.Write(info)
		mac.Write([]byte{counter})
		previous = mac.Sum(nil)
		out = append(out, previous...)
	}
	return out[:length]
}

func DeriveSFrameKeySalt(kid uint64, baseKey []byte) (key, salt []byte) {
	secret := hkdfExtract(nil, baseKey)
	suffix := make([]byte, 10)
	binary.BigEndian.PutUint64(suffix, kid)
	binary.BigEndian.PutUint16(suffix[8:], SFrameCipherSuite)
	key = hkdfExpand(secret, append([]byte("SFrame 1.0 Secret key "), suffix...), SFrameKeyBytes)
	salt = hkdfExpand(secret, append([]byte("SFrame 1.0 Secret salt "), suffix...), SFrameNonceBytes)
	return key, salt
}

func sframeNonce(salt []byte, ctr uint64) []byte {
	nonce := make([]byte, SFrameNonceBytes)
	binary.BigEndian.PutUint64(nonce[SFrameNonceBytes-8:], ctr)
	for i := range nonce {
		nonce[i] ^= salt[i]
	}
	return nonce
}

func sframeAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func SFrameEncrypt(baseKey []byte, kid, ctr uint64, metadata, plaintext []byte) ([]byte, error) {
	if len(baseKey) != SFrameKeyBytes {
		return nil, errors.New("sframe: base key must be 16 bytes")
	}
	key, salt := DeriveSFrameKeySalt(kid, baseKey)
	aead, err := sframeAEAD(key)
	if err != nil {
		return nil, err
	}
	header := EncodeSFrameHeader(kid, ctr)
	aad := append(append([]byte{}, header...), metadata...)
	return aead.Seal(append([]byte{}, header...), sframeNonce(salt, ctr), plaintext, aad), nil
}

func SFrameDecrypt(baseKeyForKID func(kid uint64) []byte, metadata, ciphertext []byte) (plaintext []byte, kid, ctr uint64, err error) {
	kid, ctr, headerLength, err := DecodeSFrameHeader(ciphertext)
	if err != nil {
		return nil, 0, 0, err
	}
	baseKey := baseKeyForKID(kid)
	if len(baseKey) != SFrameKeyBytes {
		return nil, kid, ctr, ErrSFrameNoKey
	}
	if len(ciphertext)-headerLength < SFrameTagBytes {
		return nil, kid, ctr, ErrSFrameAuthentication
	}
	key, salt := DeriveSFrameKeySalt(kid, baseKey)
	aead, err := sframeAEAD(key)
	if err != nil {
		return nil, kid, ctr, err
	}
	aad := append(append([]byte{}, ciphertext[:headerLength]...), metadata...)
	plaintext, err = aead.Open(nil, sframeNonce(salt, ctr), ciphertext[headerLength:], aad)
	if err != nil {
		return nil, kid, ctr, ErrSFrameAuthentication
	}
	return plaintext, kid, ctr, nil
}

func SFrameKID(senderSlot uint16, instance uint32, senderIndex uint16, epoch uint64) uint64 {
	return uint64(senderSlot)<<48 | uint64(instance&0xffffff)<<24 | uint64(senderIndex)<<8 | epoch&0xff
}

func SplitSFrameKID(kid uint64) (senderSlot uint16, instance uint32, senderIndex uint16, epochBits uint8) {
	return uint16(kid >> 48), uint32(kid>>24) & 0xffffff, uint16(kid >> 8), uint8(kid)
}

func mlsVarint(length int) []byte {
	switch {
	case length < 1<<6:
		return []byte{byte(length)}
	case length < 1<<14:
		return []byte{0x40 | byte(length>>8), byte(length)}
	default:
		return []byte{0x80 | byte(length>>24), byte(length >> 16), byte(length >> 8), byte(length)}
	}
}

func mlsExpandWithLabel(secret []byte, label string, context []byte, length int) []byte {
	fullLabel := append([]byte("MLS 1.0 "), label...)
	info := []byte{byte(length >> 8), byte(length)}
	info = append(info, mlsVarint(len(fullLabel))...)
	info = append(info, fullLabel...)
	info = append(info, mlsVarint(len(context))...)
	info = append(info, context...)
	return hkdfExpand(secret, info, length)
}

func MLSExporter(exporterSecret []byte, label []byte, context []byte, length int) ([]byte, error) {
	if length < 1 || length > 255*SFrameHashBytes || len(label)+8 >= 1<<30 || len(context) >= 1<<30 {
		return nil, ErrSFrameLength
	}
	derived := mlsExpandWithLabel(exporterSecret, string(label), nil, SFrameHashBytes)
	contextHash := sha256.Sum256(context)
	return mlsExpandWithLabel(derived, "exported", contextHash[:], length), nil
}

func SFrameBaseKeyFromMLS(exporterSecret []byte) []byte {
	key, _ := MLSExporter(exporterSecret, []byte(SFrameMLSExporterLabel), nil, SFrameKeyBytes)
	return key
}

func DeriveCustomerBaseKey(masterSecret []byte, sessionID string, epoch uint64) []byte {
	info := append([]byte(sessionID), 0)
	info = binary.BigEndian.AppendUint64(info, epoch)
	return hkdfExpand(hkdfExtract([]byte(SFrameCustomerKeySalt), masterSecret), info, SFrameKeyBytes)
}

const (
	SFrameCodecOpus = "opus"
	SFrameCodecVP8  = "vp8"
)

func SFrameClearPrefixLength(codec string, frame []byte) (int, error) {
	if len(frame) == 0 {
		return 0, ErrSFrameFrame
	}
	switch codec {
	case SFrameCodecOpus:
		return 0, nil
	case SFrameCodecVP8:
		prefix := 3
		if frame[0]&0x01 == 0 {
			prefix = 10
		}
		if len(frame) < prefix {
			return 0, ErrSFrameFrame
		}
		return prefix, nil
	default:
		return 0, ErrSFrameCodec
	}
}

func SFrameProtectFrame(codec string, baseKey []byte, kid, ctr uint64, frame []byte) ([]byte, error) {
	prefix, err := SFrameClearPrefixLength(codec, frame)
	if err != nil {
		return nil, err
	}
	sealed, err := SFrameEncrypt(baseKey, kid, ctr, frame[:prefix], frame[prefix:])
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, frame[:prefix]...), sealed...), nil
}

func SFrameUnprotectFrame(codec string, baseKeyForKID func(kid uint64) []byte, protected []byte) ([]byte, uint64, uint64, error) {
	prefix, err := SFrameClearPrefixLength(codec, protected)
	if err != nil {
		return nil, 0, 0, err
	}
	plaintext, kid, ctr, err := SFrameDecrypt(baseKeyForKID, protected[:prefix], protected[prefix:])
	if err != nil {
		return nil, kid, ctr, err
	}
	return append(append([]byte{}, protected[:prefix]...), plaintext...), kid, ctr, nil
}

type sframeKeySalt struct {
	baseKey []byte
	aead    cipher.AEAD
	salt    []byte
}

type sframeKeyCache map[uint64]sframeKeySalt

func (cache sframeKeyCache) get(kid uint64, baseKey []byte) (cipher.AEAD, []byte, error) {
	if entry, ok := cache[kid]; ok && hmac.Equal(entry.baseKey, baseKey) {
		return entry.aead, entry.salt, nil
	}
	key, salt := DeriveSFrameKeySalt(kid, baseKey)
	aead, err := sframeAEAD(key)
	if err != nil {
		return nil, nil, err
	}
	cache[kid] = sframeKeySalt{baseKey: append([]byte{}, baseKey...), aead: aead, salt: salt}
	return aead, salt, nil
}

type SFrameSender struct {
	mu          sync.Mutex
	Codec       string
	SenderIndex uint16
	SenderSlot  uint16
	Instance    uint32
	counters    map[uint64]uint64
	exhausted   map[uint64]bool
	keys        sframeKeyCache
}

func NewSFrameSender(codec string, senderIndex, senderSlot uint16, instance uint32) *SFrameSender {
	return &SFrameSender{Codec: codec, SenderIndex: senderIndex, SenderSlot: senderSlot, Instance: instance & 0xffffff,
		counters: map[uint64]uint64{}, exhausted: map[uint64]bool{}, keys: sframeKeyCache{}}
}

func (sender *SFrameSender) Protect(epoch uint64, baseKey, frame []byte) ([]byte, error) {
	prefix, err := SFrameClearPrefixLength(sender.Codec, frame)
	if err != nil {
		return nil, err
	}
	if len(baseKey) != SFrameKeyBytes {
		return nil, errors.New("sframe: base key must be 16 bytes")
	}
	kid := SFrameKID(sender.SenderSlot, sender.Instance, sender.SenderIndex, epoch)
	sender.mu.Lock()
	if sender.exhausted[kid] {
		sender.mu.Unlock()
		return nil, ErrSFrameExhausted
	}
	ctr := sender.counters[kid]
	if ctr == ^uint64(0) {
		sender.exhausted[kid] = true
	} else {
		sender.counters[kid] = ctr + 1
	}
	aead, salt, err := sender.keys.get(kid, baseKey)
	sender.mu.Unlock()
	if err != nil {
		return nil, err
	}
	header := EncodeSFrameHeader(kid, ctr)
	clear := frame[:prefix]
	out := append(append([]byte{}, clear...), header...)
	return aead.Seal(out, sframeNonce(salt, ctr), frame[prefix:], append(append([]byte{}, header...), clear...)), nil
}

type replayWindow struct {
	highest  uint64
	seen     map[uint64]bool
	started  bool
	floor    uint64
	hasFloor bool
}

func (window *replayWindow) accept(ctr uint64) error {
	if window.hasFloor && ctr <= window.floor {
		return ErrSFrameReplay
	}
	if !window.started {
		window.started, window.highest, window.seen = true, ctr, map[uint64]bool{ctr: true}
		return nil
	}
	if ctr+SFrameReplayWindow <= window.highest || window.seen[ctr] {
		return ErrSFrameReplay
	}
	window.seen[ctr] = true
	if ctr > window.highest {
		window.highest = ctr
		for old := range window.seen {
			if old+SFrameReplayWindow <= window.highest {
				delete(window.seen, old)
			}
		}
	}
	return nil
}

func (window *replayWindow) top() uint64 {
	if window.started && (!window.hasFloor || window.highest > window.floor) {
		return window.highest
	}
	return window.floor
}

const (
	SFrameReceiverMaxKIDs       = 256
	SFrameReceiverMaxTombstones = 4096
	SFrameReceiverRemovedEpochs = 64
)

type receiverKID struct {
	baseKey  []byte
	aead     cipher.AEAD
	salt     []byte
	window   replayWindow
	lastUsed uint64
}

type SFrameReceiver struct {
	mu            sync.Mutex
	Codec         string
	MaxKIDs       int
	MaxTombstones int
	epochs        map[uint8]sframeEpoch
	kids          map[uint64]*receiverKID
	tombstones    map[uint64]uint64
	removed       []uint64
	removedFloor  uint64
	hasFloor      bool
	clock         uint64
}

type sframeEpoch struct {
	epoch   uint64
	baseKey []byte
}

func NewSFrameReceiver(codec string) *SFrameReceiver {
	return &SFrameReceiver{Codec: codec, MaxKIDs: SFrameReceiverMaxKIDs, MaxTombstones: SFrameReceiverMaxTombstones,
		epochs: map[uint8]sframeEpoch{}, kids: map[uint64]*receiverKID{}, tombstones: map[uint64]uint64{}}
}

func (receiver *SFrameReceiver) KIDCount() int {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return len(receiver.kids)
}

func (receiver *SFrameReceiver) TombstoneCount() int {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return len(receiver.tombstones)
}

func (receiver *SFrameReceiver) SetEpoch(epoch uint64, baseKey []byte) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if receiver.wasRemovedLocked(epoch) {
		return
	}
	current, held := receiver.epochs[uint8(epoch)]
	if held && current.epoch > epoch {
		return
	}
	if held && current.epoch == epoch && hmac.Equal(current.baseKey, baseKey) {
		return
	}
	receiver.epochs[uint8(epoch)] = sframeEpoch{epoch: epoch, baseKey: append([]byte{}, baseKey...)}
	if held && current.epoch != epoch {
		receiver.markRemovedLocked(current.epoch)
		receiver.dropKIDsLocked(uint8(epoch))
	}
}

func (receiver *SFrameReceiver) RemoveEpoch(epoch uint64) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if current, ok := receiver.epochs[uint8(epoch)]; ok && current.epoch == epoch {
		delete(receiver.epochs, uint8(epoch))
		receiver.dropKIDsLocked(uint8(epoch))
		receiver.markRemovedLocked(epoch)
	}
}

func (receiver *SFrameReceiver) wasRemovedLocked(epoch uint64) bool {
	if receiver.hasFloor && epoch <= receiver.removedFloor {
		return true
	}
	for _, removed := range receiver.removed {
		if removed == epoch {
			return true
		}
	}
	return false
}

func (receiver *SFrameReceiver) markRemovedLocked(epoch uint64) {
	for _, removed := range receiver.removed {
		if removed == epoch {
			return
		}
	}
	receiver.removed = append(receiver.removed, epoch)
	if len(receiver.removed) > SFrameReceiverRemovedEpochs {
		oldest := receiver.removed[0]
		receiver.removed = append([]uint64{}, receiver.removed[1:]...)
		if !receiver.hasFloor || oldest > receiver.removedFloor {
			receiver.removedFloor, receiver.hasFloor = oldest, true
		}
	}
}

func (receiver *SFrameReceiver) dropKIDsLocked(bits uint8) {
	for kid := range receiver.kids {
		if uint8(kid) == bits {
			delete(receiver.kids, kid)
		}
	}
	for kid := range receiver.tombstones {
		if uint8(kid) == bits {
			delete(receiver.tombstones, kid)
		}
	}
}

func (receiver *SFrameReceiver) Unprotect(protected []byte) ([]byte, uint64, uint64, error) {
	prefix, err := SFrameClearPrefixLength(receiver.Codec, protected)
	if err != nil {
		return nil, 0, 0, err
	}
	kid, ctr, headerLength, err := DecodeSFrameHeader(protected[prefix:])
	if err != nil {
		return nil, 0, 0, err
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	epoch, ok := receiver.epochs[uint8(kid)]
	if !ok {
		return nil, kid, ctr, ErrSFrameNoKey
	}
	body := protected[prefix+headerLength:]
	if len(body) < SFrameTagBytes {
		return nil, kid, ctr, ErrSFrameAuthentication
	}
	state := receiver.kids[kid]
	var aead cipher.AEAD
	var salt []byte
	if state != nil && hmac.Equal(state.baseKey, epoch.baseKey) {
		aead, salt = state.aead, state.salt
	} else {
		key, derivedSalt := DeriveSFrameKeySalt(kid, epoch.baseKey)
		if aead, err = sframeAEAD(key); err != nil {
			return nil, kid, ctr, err
		}
		salt = derivedSalt
	}
	header := protected[prefix : prefix+headerLength]
	plaintext, err := aead.Open(nil, sframeNonce(salt, ctr), body, append(append([]byte{}, header...), protected[:prefix]...))
	if err != nil {
		return nil, kid, ctr, ErrSFrameAuthentication
	}
	if state == nil {
		tombstone, revived := receiver.tombstones[kid]
		if revived && ctr <= tombstone {
			return nil, kid, ctr, ErrSFrameReplay
		}
		maxKIDs, maxTombstones := receiver.MaxKIDs, receiver.MaxTombstones
		if maxKIDs <= 0 {
			maxKIDs = SFrameReceiverMaxKIDs
		}
		if maxTombstones <= 0 {
			maxTombstones = SFrameReceiverMaxTombstones
		}
		if len(receiver.kids) >= maxKIDs {
			held := len(receiver.tombstones)
			if revived {
				held--
			}
			if held >= maxTombstones {
				return nil, kid, ctr, ErrSFrameKIDLimit
			}
			var lruKID uint64
			var lru *receiverKID
			for candidate, entry := range receiver.kids {
				if lru == nil || entry.lastUsed < lru.lastUsed {
					lruKID, lru = candidate, entry
				}
			}
			delete(receiver.kids, lruKID)
			receiver.tombstones[lruKID] = lru.window.top()
		}
		delete(receiver.tombstones, kid)
		state = &receiverKID{window: replayWindow{floor: tombstone, hasFloor: revived}}
		receiver.kids[kid] = state
	}
	state.baseKey, state.aead, state.salt = epoch.baseKey, aead, salt
	if err := state.window.accept(ctr); err != nil {
		return nil, kid, ctr, err
	}
	receiver.clock++
	state.lastUsed = receiver.clock
	return append(append([]byte{}, protected[:prefix]...), plaintext...), kid, ctr, nil
}

type SFrameSessionReceivers struct {
	SessionID string
	receivers map[string]*SFrameReceiver
}

func NewSFrameSessionReceivers(sessionID string, maxKIDs, maxTombstones int) *SFrameSessionReceivers {
	receivers := map[string]*SFrameReceiver{}
	for _, codec := range []string{SFrameCodecOpus, SFrameCodecVP8} {
		receiver := NewSFrameReceiver(codec)
		if maxKIDs > 0 {
			receiver.MaxKIDs = maxKIDs
		}
		if maxTombstones > 0 {
			receiver.MaxTombstones = maxTombstones
		}
		receivers[codec] = receiver
	}
	return &SFrameSessionReceivers{SessionID: sessionID, receivers: receivers}
}

func (session *SFrameSessionReceivers) Receiver(codec string) *SFrameReceiver {
	return session.receivers[codec]
}

func (session *SFrameSessionReceivers) SetEpoch(epoch uint64, baseKey []byte) {
	for _, receiver := range session.receivers {
		receiver.SetEpoch(epoch, baseKey)
	}
}

func (session *SFrameSessionReceivers) RemoveEpoch(epoch uint64) {
	for _, receiver := range session.receivers {
		receiver.RemoveEpoch(epoch)
	}
}

func (session *SFrameSessionReceivers) Unprotect(codec string, protected []byte) ([]byte, uint64, uint64, error) {
	receiver := session.receivers[codec]
	if receiver == nil {
		return nil, 0, 0, ErrSFrameCodec
	}
	return receiver.Unprotect(protected)
}

package packager

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"sync"
	"time"
)

const (
	partRetentionSegments = 4
	memorySegments        = 8
	minimumSegments       = 6
	memoryWindowSegments  = 4 * memorySegments
)

type part struct {
	index       int
	data        []byte
	sha         string
	duration    float64
	independent bool
}

type segment struct {
	init          *initVersion
	msn           int64
	timeIndex     int64
	parts         []*part
	complete      bool
	data          []byte
	path          string
	sha           string
	bytes         int64
	duration      float64
	end           float64
	discontinuity bool
}

type initVersion struct {
	uri   string
	clear []byte
	data  []byte
	sha   string
}

type rendition struct {
	id         string
	video      bool
	h265       bool
	segmentMS  int64
	partMS     int64
	dvrS       int64
	target     int
	dir        string
	key        *cbcsKey
	starts     *segmentStarts
	follow     *segmentStarts
	waiting    []pendingFragment
	source     bool
	onSegment  func(*rendition, *segment)
	published  func(*rendition, time.Duration)
	onMSN      func(*rendition, int64)
	onSequence func(*rendition, int64)

	mu            sync.Mutex
	timescale     uint32
	inits         []*initVersion
	newInit       bool
	restarted     bool
	parent        string
	resync        bool
	codec         string
	width, height int
	fps           float64
	segments      []*segment
	nextMSN       int64
	discSeq       int
	changed       chan struct{}
	manifestSeq   int64
	manifestKey   string
	manifestCache []byte
	ready         chan struct{}
	limited       bool
	storeFailed   bool
}

func newRendition(id string, video bool, segmentMS, partMS, dvrS int64, target int, dir string, key *cbcsKey) *rendition {
	return &rendition{id: id, video: video, segmentMS: segmentMS, partMS: partMS, dvrS: dvrS, target: max(target, 1), dir: dir, key: key,
		changed: make(chan struct{}), ready: make(chan struct{})}
}

type segmentStarts struct {
	mu     sync.Mutex
	starts []segmentStart
	end    float64
}

type segmentStart struct {
	base          uint64
	timescale     uint32
	index         int64
	discontinuity bool
}

const maxSegmentStarts = 64

func (s *segmentStarts) add(start segmentStart) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, start)
	if len(s.starts) > maxSegmentStarts {
		s.starts = s.starts[len(s.starts)-maxSegmentStarts:]
	}
}

func (s *segmentStarts) reach(end float64) {
	s.mu.Lock()
	s.end = max(s.end, end)
	s.mu.Unlock()
}

func (s *segmentStarts) reached() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.end
}

func (s *segmentStarts) after(index int64) (segmentStart, bool, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, start := range s.starts {
		if start.index > index {
			return start, true, s.end
		}
	}
	return segmentStart{}, false, s.end
}

const aacHalfFrame = 512

func (start segmentStart) reachedBy(base uint64, timescale uint32) bool {
	if start.timescale == 0 || timescale == 0 {
		return false
	}
	left := new(big.Int).Mul(new(big.Int).SetUint64(base), big.NewInt(int64(start.timescale)))
	right := new(big.Int).Mul(new(big.Int).SetUint64(start.base), big.NewInt(int64(timescale)))
	right.Sub(right, big.NewInt(aacHalfFrame*int64(start.timescale)))
	return left.Cmp(right) >= 0
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

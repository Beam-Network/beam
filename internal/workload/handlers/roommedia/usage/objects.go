package usage

import (
	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

type answered struct {
	second int64
	most   string
	last   string
	lastMS int64
	audio  bool
}

type objectLog struct {
	seconds []answered
}

func (o *objectLog) add(at int64, class string) {
	second := at / 1000
	index := len(o.seconds)
	for index > 0 && o.seconds[index-1].second > second {
		index--
	}
	if index == 0 || o.seconds[index-1].second != second {
		o.seconds = append(o.seconds, answered{})
		copy(o.seconds[index+1:], o.seconds[index:])
		o.seconds[index] = answered{second: second, lastMS: none}
		index++
	}
	entry := &o.seconds[index-1]
	if class == mediacontracts.ClassAudio {
		entry.audio = true
		return
	}
	entry.most = mediacontracts.MostDemandingClass([]string{entry.most, class})
	switch {
	case at > entry.lastMS:
		entry.last, entry.lastMS = class, at
	case at == entry.lastMS:
		entry.last = mediacontracts.MostDemandingClass([]string{entry.last, class})
	}
}

func (o *objectLog) classMS(connected []mediacontracts.UsageInterval, targetMS int64) mediacontracts.ClassAmounts {
	var amounts mediacontracts.ClassAmounts
	target := targetMS / 1000
	for _, interval := range connected {
		next := 0
		lastVideo, lastAudio := none, none
		class := ""
		for second := interval[0] / 1000; second*1000 < interval[1]; second++ {
			inside := ""
			for next < len(o.seconds) && o.seconds[next].second <= second {
				entry := o.seconds[next]
				if entry.last != "" {
					lastVideo, class = entry.second, entry.last
				}
				if entry.audio {
					lastAudio = entry.second
				}
				if entry.second == second {
					inside = entry.most
				}
				next++
			}
			part := min(interval[1], (second+1)*1000) - max(interval[0], second*1000)
			switch {
			case inside != "":
				amounts.Add(inside, part)
			case lastVideo != none && lastVideo >= second-target:
				amounts.Add(class, part)
			case lastAudio != none && lastAudio >= second-target:
				amounts.Add(mediacontracts.ClassAudio, part)
			}
		}
	}
	return amounts
}

func (o *objectLog) before(at int64) {
	for len(o.seconds) > 0 && o.seconds[len(o.seconds)-1].second > at/1000 {
		o.seconds = o.seconds[:len(o.seconds)-1]
	}
}

func (o *objectLog) keep(at, targetMS int64) {
	first := (at - targetMS) / 1000
	drop := 0
	for drop < len(o.seconds) && o.seconds[drop].second < first {
		drop++
	}
	o.seconds = append(o.seconds[:0], o.seconds[drop:]...)
}

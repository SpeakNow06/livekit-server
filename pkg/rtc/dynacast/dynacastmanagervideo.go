// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dynacast

import (
	"maps"
	"time"

	"github.com/bep/debounce"

	"github.com/livekit/protocol/codecs/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

var _ DynacastManager = (*dynacastManagerVideo)(nil)
var _ dynacastQualityListener = (*dynacastManagerVideo)(nil)

type DynacastManagerVideoParams struct {
	DynacastPauseDelay time.Duration
	Listener           DynacastManagerListener
	Logger             logger.Logger
	// SpeakNow fork #6: exact-match (yalniz izlenen katmani ac) YALNIZ cok-katmanli (simulcast,
	// >1 layer) track'lerde guvenli. Tek-katmanli track'te (kamera) tek encoding farkli kalite
	// slotunda olabilir (orn. rid "q"=LOW iken server quality'i HIGH sanir) -> exact-match o tek
	// encoding'i kapatir -> kamera olur. Bu fonksiyon false donerse (veya nil) stock'a dusulur.
	IsMultiLayer func(mimeType mime.MimeType) bool
}

type dynacastManagerVideo struct {
	params DynacastManagerVideoParams

	maxSubscribedQuality          map[mime.MimeType]livekit.VideoQuality
	committedMaxSubscribedQuality map[mime.MimeType]livekit.VideoQuality
	// SpeakNow fork #6: max'in yani sira aboneli katman KUMESI (exact-match dynacast).
	subscribedQualities          map[mime.MimeType]qualitySet
	committedSubscribedQualities map[mime.MimeType]qualitySet

	maxSubscribedQualityDebounce        func(func())
	maxSubscribedQualityDebouncePending bool

	isClosed bool

	*dynacastManagerBase
}

func NewDynacastManagerVideo(params DynacastManagerVideoParams) DynacastManager {
	if params.Logger == nil {
		params.Logger = logger.GetLogger()
	}
	d := &dynacastManagerVideo{
		params:                        params,
		maxSubscribedQuality:          make(map[mime.MimeType]livekit.VideoQuality),
		committedMaxSubscribedQuality: make(map[mime.MimeType]livekit.VideoQuality),
		subscribedQualities:           make(map[mime.MimeType]qualitySet), // fork #6
		committedSubscribedQualities:  make(map[mime.MimeType]qualitySet), // fork #6
	}
	if params.DynacastPauseDelay > 0 {
		d.maxSubscribedQualityDebounce = debounce.New(params.DynacastPauseDelay)
	}
	d.dynacastManagerBase = newDynacastManagerBase(dynacastManagerBaseParams{
		Logger:        params.Logger,
		OpsQueueDepth: 64,
		OnRestart: func() {
			d.committedMaxSubscribedQuality = make(map[mime.MimeType]livekit.VideoQuality)
			d.committedSubscribedQualities = make(map[mime.MimeType]qualitySet) // fork #6
		},
		OnDynacastQualityCreate: func(mimeType mime.MimeType) dynacastQuality {
			dq := newDynacastQualityVideo(dynacastQualityVideoParams{
				MimeType: mimeType,
				Listener: d,
				Logger:   d.params.Logger,
			})
			return dq
		},
		OnRegressCodec: func(fromMime, toMime mime.MimeType) {
			d.maxSubscribedQuality[fromMime] = livekit.VideoQuality_OFF
			d.subscribedQualities[fromMime] = 0 // fork #6

			// if the new codec is not added, notify the publisher to start publishing
			if _, ok := d.maxSubscribedQuality[toMime]; !ok {
				d.maxSubscribedQuality[toMime] = livekit.VideoQuality_HIGH
				// fork #6: HIGH'a zorlarken kume'yi de TAM doldur — gercek bildirim (Replace
				// sonrasi) gelene dek tum katmanlar yayinlansin (upstream "force HIGH" ile ayni niyet).
				var full qualitySet
				full.addUpTo(livekit.VideoQuality_HIGH)
				d.subscribedQualities[toMime] = full
			}
		},
		OnUpdateNeeded: d.update,
	})
	return d
}

// It is possible for tracks to be in pending close state. When track
// is waiting to be closed, a node is not streaming a track. This can
// be used to force an update announcing that subscribed quality is OFF,
// i.e. indicating not pulling track any more.
func (d *dynacastManagerVideo) ForceQuality(quality livekit.VideoQuality) {
	d.lock.Lock()
	defer d.lock.Unlock()

	for mime := range d.committedMaxSubscribedQuality {
		d.committedMaxSubscribedQuality[mime] = quality
		// fork #6: kume'yi de senkron tut (ForceQuality genelde OFF=pending-close; non-OFF'ta <=quality)
		var set qualitySet
		if quality != livekit.VideoQuality_OFF {
			set.addUpTo(quality)
		}
		d.committedSubscribedQualities[mime] = set
	}

	d.enqueueSubscribedQualityChange()
}

func (d *dynacastManagerVideo) NotifySubscriberMaxQuality(
	subscriberID livekit.ParticipantID,
	mime mime.MimeType,
	quality livekit.VideoQuality,
) {
	dq := d.getOrCreateDynacastQuality(mime)
	if dq != nil {
		dq.NotifySubscriberMaxQuality(subscriberID, quality)
	}
}

func (d *dynacastManagerVideo) NotifySubscriberNodeMaxQuality(
	nodeID livekit.NodeID,
	qualities []types.SubscribedCodecQuality,
) {
	for _, quality := range qualities {
		dq := d.getOrCreateDynacastQuality(quality.CodecMime)
		if dq != nil {
			dq.NotifySubscriberNodeMaxQuality(nodeID, quality.Quality)
		}
	}
}

func (d *dynacastManagerVideo) OnUpdateMaxQualityForMime(
	mime mime.MimeType,
	maxQuality livekit.VideoQuality,
	qualities qualitySet, // fork #6
) {
	d.lock.Lock()
	if _, ok := d.regressedCodec[mime]; !ok {
		d.maxSubscribedQuality[mime] = maxQuality
		d.subscribedQualities[mime] = qualities // fork #6
	}
	d.lock.Unlock()

	d.update(false)
}

func (d *dynacastManagerVideo) update(force bool) {
	d.lock.Lock()

	d.params.Logger.Debugw(
		"processing quality change",
		"force", force,
		"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality,
	)

	if len(d.maxSubscribedQuality) == 0 {
		// no mime has been added, nothing to update
		d.lock.Unlock()
		return
	}

	// add or remove of a mime triggers an update
	changed := len(d.maxSubscribedQuality) != len(d.committedMaxSubscribedQuality)
	downgradesOnly := !changed
	if !changed {
		for mime, quality := range d.maxSubscribedQuality {
			if cq, ok := d.committedMaxSubscribedQuality[mime]; ok {
				// SpeakNow fork #6: max ayni kalsa bile aboneli katman kumesi degisebilir
				// (orn. izlenmeyen orta katman bosaldi) -> bu da update tetikler.
				set := d.subscribedQualities[mime]
				cset := d.committedSubscribedQualities[mime]
				if cq != quality || set != cset {
					changed = true
				}

				if (cq == livekit.VideoQuality_OFF && quality != livekit.VideoQuality_OFF) || (cq != livekit.VideoQuality_OFF && quality != livekit.VideoQuality_OFF && cq < quality) {
					downgradesOnly = false
				}
				// fork #6: kume YENI bit kazandiysa (bir katman yeniden gerekiyor) bu bir
				// "upgrade" -> debounce'suz aninda gonder (izleyici o katmani simdi bekliyor).
				if set&^cset != 0 {
					downgradesOnly = false
				}
			}
		}
	}

	if !force {
		if !changed {
			d.lock.Unlock()
			return
		}

		if downgradesOnly && d.maxSubscribedQualityDebounce != nil {
			if !d.maxSubscribedQualityDebouncePending {
				d.params.Logger.Debugw(
					"debouncing quality downgrade",
					"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
					"maxSubscribedQuality", d.maxSubscribedQuality,
				)
				d.maxSubscribedQualityDebounce(func() {
					d.update(true)
				})
				d.maxSubscribedQualityDebouncePending = true
			} else {
				d.params.Logger.Debugw(
					"quality downgrade waiting for debounce",
					"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
					"maxSubscribedQuality", d.maxSubscribedQuality,
				)
			}
			d.lock.Unlock()
			return
		}
	}

	// clear debounce on send
	if d.maxSubscribedQualityDebounce != nil {
		d.maxSubscribedQualityDebounce(func() {})
		d.maxSubscribedQualityDebouncePending = false
	}

	d.params.Logger.Debugw(
		"committing quality change",
		"force", force,
		"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality,
	)

	// commit change
	d.committedMaxSubscribedQuality = make(map[mime.MimeType]livekit.VideoQuality, len(d.maxSubscribedQuality))
	maps.Copy(d.committedMaxSubscribedQuality, d.maxSubscribedQuality)
	// SpeakNow fork #6: kume'yi de commit et.
	d.committedSubscribedQualities = make(map[mime.MimeType]qualitySet, len(d.subscribedQualities))
	maps.Copy(d.committedSubscribedQualities, d.subscribedQualities)

	d.enqueueSubscribedQualityChange()
	d.lock.Unlock()
}

func (d *dynacastManagerVideo) enqueueSubscribedQualityChange() {
	if d.isClosed || d.params.Listener == nil {
		return
	}

	subscribedCodecs := make([]*livekit.SubscribedCodec, 0, len(d.committedMaxSubscribedQuality))
	maxSubscribedQualities := make([]types.SubscribedCodecQuality, 0, len(d.committedMaxSubscribedQuality))
	for mime, quality := range d.committedMaxSubscribedQuality {
		maxSubscribedQualities = append(maxSubscribedQualities, types.SubscribedCodecQuality{
			CodecMime: mime,
			Quality:   quality,
		})

		if quality == livekit.VideoQuality_OFF {
			subscribedCodecs = append(subscribedCodecs, &livekit.SubscribedCodec{
				Codec: mime.String(),
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			})
		} else {
			// ESKİ (upstream): her q <= quality acilir — max'in altindaki katmanlar izlenmese
			//   de encode edilir (sicak tutulup aninda dususe izin verir):
			//   Enabled: q <= quality
			// SpeakNow fork #6: YALNIZ gercekten aboneli katmanlar (committed kume) acilir
			//   (Meet gibi: tek izleyici 1080 -> 720/540 paused). Guard: kume bos ama max!=OFF
			//   (ForceQuality/regress yarisi) -> guvenli tarafta upstream davranisina dus.
			// SpeakNow fork #6: exact-match yalniz cok-katmanli track'te. Tek-katman (kamera) ya da
			// kume bos (ForceQuality/regress yarisi) -> stock (q<=quality), tek encoding'i asla kapatma.
			multiLayer := d.params.IsMultiLayer != nil && d.params.IsMultiLayer(mime)
			set := d.committedSubscribedQualities[mime]
			var subscribedQualities []*livekit.SubscribedQuality
			for q := livekit.VideoQuality_LOW; q <= livekit.VideoQuality_HIGH; q++ {
				var enabled bool
				if multiLayer && set != 0 {
					enabled = set.has(q) // cok-katman -> exact-match (izlenmeyen alt katman paused)
				} else {
					enabled = q <= quality // tek-katman/guard -> stock (hep guvenli)
				}
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{
					Quality: q,
					Enabled: enabled,
				})
			}
			subscribedCodecs = append(subscribedCodecs, &livekit.SubscribedCodec{
				Codec:     mime.String(),
				Qualities: subscribedQualities,
			})
		}
	}

	d.params.Logger.Debugw(
		"subscribedMaxQualityChange",
		"subscribedCodecs", subscribedCodecs,
		"maxSubscribedQualities", maxSubscribedQualities,
	)
	d.notifyOpsQueue.Enqueue(func() {
		d.params.Listener.OnDynacastSubscribedMaxQualityChange(subscribedCodecs, maxSubscribedQualities)
	})
}

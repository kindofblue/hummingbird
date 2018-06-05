//  Copyright (c) 2015 Rackspace
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
//  implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package objectserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/troubling/hummingbird/common/fs"
	"github.com/troubling/hummingbird/common/ring"
	"github.com/uber-go/tally"
)

const nurseryObjectSleep = 10 * time.Millisecond

type nurseryDevice struct {
	r         *Replicator
	passStart time.Time
	dev       *ring.Device
	policy    int
	oring     ring.Ring
	canchan   chan struct{}
	objEngine NurseryObjectEngine

	stabilizationAttemptsMetric         tally.Counter
	stabilizationSuccessesMetric        tally.Counter
	stabilizationFailuresMetric         tally.Counter
	stabilizationLastPassCountMetric    tally.Gauge
	stabilizationLastPassDurationMetric tally.Timer
}

type PriorityReplicationResult struct {
	ObjectsReplicated int64
	ObjectsErrored    int64
	Success           bool
	ErrorMsg          string
}

func (nrd *nurseryDevice) UpdateStat(stat string, amount int64) {
	nrd.r.updateStat <- statUpdate{nrd.Type(), nrd.Key(), stat, amount}
}

func (nrd *nurseryDevice) Scan() {
	nrd.UpdateStat("startRun", 1)
	if mounted, err := fs.IsMount(filepath.Join(nrd.r.deviceRoot, nrd.dev.Device)); nrd.r.checkMounts && (err != nil || mounted != true) {
		nrd.r.logger.Error("[stabilizeDevice] Drive not mounted", zap.String("Device", nrd.dev.Device), zap.Error(err))
		return
	}
	start := time.Now()
	c := make(chan ObjectStabilizer, 100)
	cancel := make(chan struct{})
	defer close(cancel)
	go nrd.objEngine.GetObjectsToStabilize(nrd.dev.Device, c, cancel)
	count := 0
	for o := range c {
		count++
		nrd.stabilizationAttemptsMetric.Inc(1)
		nrd.UpdateStat("checkin", 1)
		func() {
			nrd.r.nurseryConcurrencySem <- struct{}{}
			defer func() {
				<-nrd.r.nurseryConcurrencySem
			}()
			if err := o.Stabilize(nrd.dev); err == nil {
				nrd.stabilizationSuccessesMetric.Inc(1)
				nrd.UpdateStat("ObjectsStabilizedSuccess", 1)
				nrd.UpdateStat("ObjectsStabilizedBytes", o.ContentLength())
			} else {
				nrd.stabilizationFailuresMetric.Inc(1)
				nrd.r.logger.Debug("[stabilizeDevice] error Stabilize obj", zap.String("Object", o.Repr()), zap.Error(err))
				nrd.UpdateStat("ObjectsStabilizedError", 1)
			}
		}()
		select {
		case <-time.After(nurseryObjectSleep):
		case <-nrd.canchan:
			return
		}
	}
	nrd.stabilizationLastPassCountMetric.Update(float64(count))
	// We don't use Tally's Timer Start().Stop() since we don't want to record canceled passes.
	nrd.stabilizationLastPassDurationMetric.Record(time.Since(start))
	nrd.UpdateStat("PassComplete", 1)
	nrd.r.logger.Info("[stabilizeDevice] Pass complete.")
}

func (nrd *nurseryDevice) ScanLoop() {
	for {
		select {
		case <-nrd.canchan:
			return
		default:
			nrd.Scan()
			time.Sleep(10 * time.Second)
		}
	}
}

func (nrd *nurseryDevice) Cancel() {
	close(nrd.canchan)
}

func (nrd *nurseryDevice) Key() string {
	return deviceKeyId(nrd.dev.Device, nrd.policy)
}

func (nrd *nurseryDevice) Type() string {
	return "object-nursery"
}

func (nrd *nurseryDevice) PriorityReplicate(w http.ResponseWriter, pri PriorityRepJob) {
	objc := make(chan ObjectStabilizer)
	canchan := make(chan struct{})

	go nrd.objEngine.GetObjectsToReplicate(pri, objc, canchan)
	//TODO: add concurrency to this
	w.WriteHeader(http.StatusOK)
	t := time.Now()
	prr := PriorityReplicationResult{}
	for o := range objc {
		if err := o.Replicate(pri); err != nil {
			nrd.r.logger.Error("error prirep Replicate", zap.Error(err))
			prr.ObjectsErrored++
			nrd.UpdateStat("ObjectsReplicatedError", 1)
		} else {
			prr.ObjectsReplicated++
			nrd.UpdateStat("ObjectsReplicatedSuccess", 1)
			nrd.UpdateStat("ObjectsReplicatedBytes", o.ContentLength())
			if time.Since(t) > time.Minute {
				w.Write([]byte(" "))
				t = time.Now()
			}
		}
	}
	prr.Success = prr.ObjectsErrored == 0
	if !prr.Success {
		prr.ErrorMsg = fmt.Sprintf("%d objects failed to replicate", prr.ObjectsErrored)
	}
	b, err := json.Marshal(prr)
	if err != nil {
		nrd.r.logger.Error("error prirep jsoning", zap.Error(err))
		b = []byte("There was an internal server error generating JSON.")
	}
	w.Write(b)
	w.Write([]byte("\n"))
	nrd.UpdateStat("PriorityRepsDone", 1)
}

func GetNurseryDevice(oring ring.Ring, dev *ring.Device, policy int, r *Replicator, f NurseryObjectEngine) (ReplicationDevice, error) {
	nrd := &nurseryDevice{
		r:         r,
		dev:       dev,
		policy:    policy,
		oring:     oring,
		passStart: time.Now(),
		canchan:   make(chan struct{}),
		objEngine: f,
	}
	nrd.stabilizationAttemptsMetric = r.metricsScope.Counter(dev.Device + "_stabilization_attempts")
	nrd.stabilizationSuccessesMetric = r.metricsScope.Counter(dev.Device + "_stabilization_successes")
	nrd.stabilizationFailuresMetric = r.metricsScope.Counter(dev.Device + "_stabilization_failures")
	nrd.stabilizationLastPassCountMetric = r.metricsScope.Gauge(dev.Device + "_stabilization_last_pass_count")
	nrd.stabilizationLastPassDurationMetric = r.metricsScope.Timer(dev.Device + "_stabilization_last_pass_duration")
	return nrd, nil
}

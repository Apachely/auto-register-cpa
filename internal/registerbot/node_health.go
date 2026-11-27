package registerbot

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// startNodeHealthLoop periodically measures which gateway nodes can currently
// reach the identity provider. The sweep reads the controller only, so it never
// moves a selector an account is holding, and it is skipped entirely while any
// row is in flight. This replaces the previous behaviour where one startup
// snapshot decided the selectable pool for the whole process lifetime.
func (e *Engine) startNodeHealthLoop() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			interval := e.Settings().Gateway.NodeProbeInterval()
			if interval <= 0 {
				return
			}
			timer := time.NewTimer(interval)
			select {
			case <-e.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if e.hasBusyWork() {
				continue
			}
			e.probeNodeHealth(e.ctx)
		}
	}()
}

// hasBusyWork reports whether any account row is currently running. Background
// health work must never overlap an active route lease.
func (e *Engine) hasBusyWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, run := range e.runs {
		for _, row := range run.Rows {
			if row.busy {
				return true
			}
		}
	}
	return false
}

// probeNodeHealth sweeps every automatic channel group once and folds the outcome
// into the scheduler ledger: reachable nodes leave cooldown, unreachable ones are
// cooled down and pushed to the back of the rotation.
func (e *Engine) probeNodeHealth(ctx context.Context) {
	if ctx == nil {
		ctx = e.ctx
	}
	settings := e.Settings()
	// A cheap non-forced probe guarantees the group table exists before it is swept.
	e.gateway.Probe(ctx, false)
	for _, channel := range settings.Channels {
		if !channel.Enabled || channel.ManualMode || ctx.Err() != nil {
			continue
		}
		nodes := uniqueNodes(e.gateway.NodePool(channel))
		if len(nodes) == 0 {
			continue
		}
		result := e.gateway.ProbeNodes(ctx, channel.Group, nodes)
		if result.Total == 0 {
			continue
		}
		promoted, cooled, errApply := e.proxyPool.ApplyProbeResults(result, time.Now())
		if errApply != nil {
			e.bus.Emit(Event{Channel: channel.ID, Stage: StageNodeHealth, Level: "warning", Message: "node_health_state_failed"})
			log.WithError(errApply).WithField("component", "registerbot").Warn("node health ledger update failed")
			continue
		}
		if promoted == 0 && cooled == 0 {
			continue
		}
		message := fmt.Sprintf("node_health channel=%s reachable=%d/%d promoted=%d cooled=%d", channel.ID, len(result.Reachable), result.Total, promoted, cooled)
		level := "info"
		if len(result.Reachable) == 0 {
			level = "warning"
		}
		e.bus.Emit(Event{Channel: channel.ID, Stage: StageNodeHealth, Level: level, Message: message})
		log.WithFields(log.Fields{
			"component": "registerbot",
			"channel":   channel.ID,
			"reachable": len(result.Reachable),
			"total":     result.Total,
			"promoted":  promoted,
			"cooled":    cooled,
		}).Info("registerbot node health sweep completed")
	}
}

// RefreshNodeHealth runs one immediate sweep and returns the updated ledger view.
// The management refresh path uses it so an operator action always measures live.
func (e *Engine) RefreshNodeHealth(ctx context.Context) ProxyPoolSnapshot {
	e.probeNodeHealth(ctx)
	return e.proxyPool.Snapshot()
}

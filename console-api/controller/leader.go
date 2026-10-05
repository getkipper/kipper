package controller

import (
	"context"

	"github.com/getkipper/kipper/console-api/internal/leader"
)

// autoSizerLease coordinates tuning across console-api pods during rollouts
// to avoid duplicate adjustments and alerts during normal operation.
const autoSizerLease = "kipper-resource-auto-sizer"

// RunAsLeader runs the resource controller only while this pod holds the
// auto-sizer Lease. identity names this pod. It blocks until ctx is
// cancelled.
func (rc *ResourceController) RunAsLeader(ctx context.Context, identity string) {
	leader.Run(ctx, rc.client, modeConfigMapNamespace, autoSizerLease, identity, leader.DefaultTiming, &rc.leader, rc.Run)
}

package handlers

import (
	"context"
	"errors"
	"net/http"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/workloadname"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

// reserveWorkloadName takes name for a workload of kind creating, and answers
// the request itself when it cannot.
//
// It returns the release to run if the workload write then fails, and whether
// the caller should carry on. The reservation lives in workloadname because the
// environment-copy and promotion paths create workloads too, from other
// packages.
func reserveWorkloadName(ctx context.Context, w http.ResponseWriter, c crclient.Client, namespace, name, creating string) (release func(), ok bool) {
	release, err := workloadname.Reserve(ctx, c, namespace, name, creating)
	if err != nil {
		respondWorkloadNameError(w, err)
		return func() {}, false
	}
	return release, true
}

// respondWorkloadNameError answers a failed availability check.
//
// Only a name another kind holds is a conflict. A lookup that could not
// complete is a server failure, and answering it with 409 would tell a client
// to correct a request that was never wrong.
func respondWorkloadNameError(w http.ResponseWriter, err error) {
	var taken workload.NameTakenError
	if errors.As(err, &taken) {
		respondError(w, http.StatusConflict, err.Error())
		return
	}
	respondError(w, http.StatusInternalServerError, err.Error())
}

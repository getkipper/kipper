package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/kip/internal/clusteridentity"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/domain"
	"github.com/getkipper/kipper/kip/internal/installer"
)

const (
	gatewayCredentialsSecret    = "gateway-credentials"
	gatewayCredentialsNamespace = "kipper-system"
	gatewayCredentialsKey       = "token"

	// An in-flight label move is recorded in the same Secret: the previous
	// registration's label and token plus the destination label they belong
	// to, kept until the move's cleanup deregisters the old label. Persisting
	// them here (not in CLI process memory) is what lets an interrupted
	// 'kip cluster domain' be finished by any later run, including --sync.
	// The destination is recorded so a later move to a different target can
	// never mistake this record for its own.
	gatewayCredentialsOldLabelKey = "old-label"
	gatewayCredentialsOldTokenKey = "old-token"
	gatewayCredentialsNewLabelKey = "new-label"
)

// isKipperRunDomain reports whether a domain is a *.kipper.run subdomain.
// Canonicalised first, because DNS names are case-insensitive and may be written
// fully qualified: LAB.KIPPER.RUN and lab.kipper.run. name the same host, and a
// raw suffix test read both as custom domains and skipped the gateway claim.
func isKipperRunDomain(d string) bool {
	return strings.HasSuffix(installer.NormaliseDomain(d), ".kipper.run")
}

// kipperRunLabel returns the single label of a *.kipper.run domain.
func kipperRunLabel(d string) string {
	return strings.TrimSuffix(installer.NormaliseDomain(d), ".kipper.run")
}

// beginGatewayMove claims the target *.kipper.run label for this cluster before
// any cluster mutation, when the target is a kipper.run domain the cluster is
// not already on. It returns the gateway fragment to fold into the spec patch,
// or nil when no gateway action is needed (a custom domain, or the cluster's
// current domain).
func beginGatewayMove(clientset kubernetes.Interface, cluster *config.Cluster, current *clusteridentity.ClusterIdentity, targetDomain string) (map[string]any, error) {
	if !isKipperRunDomain(targetDomain) || targetDomain == current.Spec.Domain {
		return nil, nil
	}
	if err := registerGatewayLabel(clientset, "", cluster.Host, cluster.Name, targetDomain, current); err != nil {
		return nil, err
	}
	return map[string]any{"kipperRunDomain": targetDomain, "register": true}, nil
}

// registerGatewayLabel claims the target label for this cluster and records the
// move durably: the freshly-issued token and the previous registration's label
// and token land in the gateway-credentials Secret in one write, before any
// other cluster change. Any later kip run can finish the cleanup from the
// Secret alone via finishGatewayMove. The gateway only returns a token on a
// first registration; a same-IP renewal returns none, which means the cluster
// already owns the label and the stored token still stands. The one exposure
// left is the instant between the gateway call and the Secret write: a crash
// exactly there loses the new token, because the gateway never re-discloses it.
func registerGatewayLabel(clientset kubernetes.Interface, gwURL, serverIP, clusterName, newDomain string, current *clusteridentity.ClusterIdentity) error {
	label := kipperRunLabel(newDomain)
	if err := hostnames.ValidateClusterLabel(label); err != nil {
		return fmt.Errorf("invalid *.kipper.run label %q: %w", label, err)
	}

	ctx := context.Background()
	data, err := readGatewayCredentials(ctx, clientset)
	if err != nil {
		return err
	}

	// A recorded move for THIS destination wins over re-capturing: after an
	// interrupted run the token key already holds the new label's token, and
	// capturing that as "old" would later deregister the very label being
	// moved to. A record for a DIFFERENT destination must be finished first —
	// starting a second move on top of it would clobber the token that record
	// still needs, so refusal beats a corrupted journal.
	oldLabel := string(data[gatewayCredentialsOldLabelKey])
	oldToken := string(data[gatewayCredentialsOldTokenKey])
	recordedFor := string(data[gatewayCredentialsNewLabelKey])
	if (oldLabel != "" || oldToken != "") && recordedFor != label {
		if err := finishGatewayMove(clientset, gwURL); err != nil {
			return fmt.Errorf("an earlier domain change still has gateway cleanup pending (removing %s.kipper.run); it must finish before another move starts: %w", oldLabel, err)
		}
		if data, err = readGatewayCredentials(ctx, clientset); err != nil {
			return err
		}
		oldLabel, oldToken = "", ""
	}
	if oldLabel == "" && oldToken == "" {
		oldLabel = kipperRunLabel(current.KipperRunDomain())
		oldToken = string(data[gatewayCredentialsKey])
	}

	// Present the token when this cluster has a claim to the label already:
	// either it is the name the cluster currently serves on (a renewal, not a
	// move), or the target of a move an earlier attempt registered before dying.
	// Sending it is what lets the gateway authenticate us; whether it did is
	// decided below by the challenge it returns, never by the fact that we sent
	// something.
	known := ""
	if label == kipperRunLabel(current.KipperRunDomain()) || string(data[gatewayCredentialsNewLabelKey]) == label {
		known = string(data[gatewayCredentialsKey])
	}

	gw := domain.NewGatewayClient(gwURL)
	reg, err := gw.Register(label, serverIP, known)
	if err != nil {
		// A conflict means the name is taken; anything else is the gateway
		// having trouble, and telling an operator to pick a different name for
		// what a retry would fix sends them to rename a cluster needlessly.
		if errors.Is(err, domain.ErrNameTaken) {
			return fmt.Errorf("could not register %s with the gateway (it may already belong to another server): %w", newDomain, err)
		}
		return fmt.Errorf("the gateway could not be asked about %s: %w", newDomain, err)
	}

	// The gateway discloses a token only when it creates a registration, and
	// answers every other outcome without one — a renewal it authorised and a
	// request it turned away look identical on the wire. What separates them is
	// the challenge: it is issued only to a caller whose token the gateway
	// recognised, so its presence is the proof of acceptance, where a token this
	// side merely sent proves nothing. A stale credential would otherwise read
	// as ownership.
	//
	// Without that proof, continuing would carry the old label's token onto the
	// new name: the cluster could never renew or prove the target, while the
	// move deregisters the label that currently works — a route discarded
	// without its replacement acquired.
	if reg.Token == "" && reg.Challenge == "" {
		return fmt.Errorf("the subdomain %s is already registered to another cluster, or to an installation whose token this machine no longer holds, "+
			"so moving onto it would leave this cluster unable to prove the name. Choose a different subdomain, "+
			"or move to a domain you control: kip cluster domain kipper.example.com", newDomain)
	}

	if reg.Token != "" {
		data[gatewayCredentialsKey] = []byte(reg.Token)
	}
	if oldLabel != "" && oldToken != "" && oldLabel != label {
		data[gatewayCredentialsOldLabelKey] = []byte(oldLabel)
		data[gatewayCredentialsOldTokenKey] = []byte(oldToken)
		data[gatewayCredentialsNewLabelKey] = []byte(label)
	} else {
		delete(data, gatewayCredentialsOldLabelKey)
		delete(data, gatewayCredentialsOldTokenKey)
		delete(data, gatewayCredentialsNewLabelKey)
	}
	if err := writeGatewayCredentials(ctx, clientset, data); err != nil {
		return err
	}
	if reg.Token != "" {
		// Best effort here, unlike the uninstall path: the cluster's own Secret
		// was just written above and remains the authoritative copy, so a
		// missing local mirror costs recoverability rather than the name itself.
		// Replace what is recorded now, and give up rather than fight a command
		// that changes it underneath — this copy is a convenience, since the
		// Secret written above is the authority.
		_ = mirrorGatewayTokenToConfig(clusterName, mirroredGatewayToken(clusterName), reg.Token)
	}
	return nil
}

// finishGatewayMove deregisters the previous *.kipper.run registration recorded
// in the gateway-credentials Secret by an earlier move, then clears the record.
// Safe to call on any converged cluster: with no recorded move it does nothing.
// A failed deregistration keeps the record so a later run (--sync) can retry.
// gwURL selects the gateway endpoint; empty means the default public gateway.
func finishGatewayMove(clientset kubernetes.Interface, gwURL string) error {
	ctx := context.Background()
	data, err := readGatewayCredentials(ctx, clientset)
	if err != nil {
		return err
	}
	oldLabel := string(data[gatewayCredentialsOldLabelKey])
	oldToken := string(data[gatewayCredentialsOldTokenKey])
	if oldLabel == "" && oldToken == "" {
		return nil
	}
	// ErrNotRegistered means a previous attempt already deregistered the label
	// but died before clearing the record — cleanup is done, only the record
	// remains. Anything else keeps the record so a later run retries.
	err = domain.NewGatewayClient(gwURL).Deregister(oldToken)
	switch {
	case errors.Is(err, domain.ErrNotRegistered):
		fmt.Printf("  ✔  The old subdomain %s.kipper.run was already removed\n", oldLabel)
	case err != nil:
		return fmt.Errorf("could not remove the old subdomain %s.kipper.run from the gateway: %w", oldLabel, err)
	default:
		fmt.Printf("  ✔  Removed the old subdomain %s.kipper.run\n", oldLabel)
	}
	delete(data, gatewayCredentialsOldLabelKey)
	delete(data, gatewayCredentialsOldTokenKey)
	delete(data, gatewayCredentialsNewLabelKey)
	return writeGatewayCredentials(ctx, clientset, data)
}

// clusterGatewayToken returns the current token from the gateway-credentials
// Secret, or empty when it cannot be read. Best-effort by design: local-config
// repair must still fix hosts on a cluster whose Secret is unreadable.
func clusterGatewayToken(clientset kubernetes.Interface) string {
	data, err := readGatewayCredentials(context.Background(), clientset)
	if err != nil {
		return ""
	}
	return string(data[gatewayCredentialsKey])
}

// ErrMirrorHolds reports that the entry records something other than the value
// the caller expected, so nothing was written over it.
var ErrMirrorHolds = errors.New("the local entry records a different gateway credential")

// mirrorGatewayTokenToConfig records a token in the local entry, replacing
// expected.
//
// It is a compare-and-swap rather than a plain write because "this came off the
// cluster, so it wins" is not true for long. It is authoritative for the
// registration that was read, at the moment it was read; a `kip cluster domain`
// move finishing in the gap between that read and this write leaves a newer
// credential under the same name, and overwriting it discards the only local
// copy of a live one. The config lock cannot help — it orders writers, and the
// stale value came from a Kubernetes secret nothing here locks.
//
// expected is what the caller saw before it went looking, so an empty expected
// means "write only into an entry holding nothing", which is what the retry
// after a refused release needs.
func mirrorGatewayTokenToConfig(clusterName, expected, token string) error {
	return config.Update(func(cfg *config.Config) error {
		c := cfg.GetCluster(clusterName)
		if c == nil {
			// Nothing to mirror into. Saying so matters: the caller tells an
			// operator their credential is recorded, and reporting success here
			// made that a lie for an entry another command had removed.
			return fmt.Errorf("cluster %q is not in the local config", clusterName)
		}
		if c.GatewayToken == token {
			return config.ErrNoChange
		}
		if c.GatewayToken != expected {
			return fmt.Errorf("%w: %s", ErrMirrorHolds, clusterName)
		}
		c.GatewayToken = token
		return nil
	})
}

// readGatewayCredentials returns a mutable copy of the gateway-credentials
// Secret data, or an empty map when the Secret does not exist yet.
func readGatewayCredentials(ctx context.Context, clientset kubernetes.Interface) (map[string][]byte, error) {
	s, err := clientset.CoreV1().Secrets(gatewayCredentialsNamespace).Get(ctx, gatewayCredentialsSecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string][]byte{}, nil
		}
		return nil, fmt.Errorf("reading gateway credentials: %w", err)
	}
	if s.Data == nil {
		return map[string][]byte{}, nil
	}
	return s.Data, nil
}

// writeGatewayCredentials upserts the gateway-credentials Secret with the given
// data in one write, so the token and any recorded move stay consistent.
func writeGatewayCredentials(ctx context.Context, clientset kubernetes.Interface, data map[string][]byte) error {
	secrets := clientset.CoreV1().Secrets(gatewayCredentialsNamespace)
	existing, err := secrets.Get(ctx, gatewayCredentialsSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, createErr := secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
			Data:       data,
		}, metav1.CreateOptions{})
		if createErr != nil {
			return fmt.Errorf("storing gateway credentials: %w", createErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading gateway credentials: %w", err)
	}
	existing.Data = data
	if _, err := secrets.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating gateway credentials: %w", err)
	}
	return nil
}

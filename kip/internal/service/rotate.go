package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

// EngineCommand is a command run inside a service pod. Passwords travel only
// on Stdin, so they never appear in a process's arguments.
type EngineCommand struct {
	Argv  []string
	Stdin string
}

// RotatedKey names the credentials Secret key that holds the password a
// rotation replaces. It returns SECRET_KEY for MinIO and PASSWORD for all other
// types, without validating the service type.
func RotatedKey(svcType string) string {
	if svcType == "minio" {
		return "SECRET_KEY"
	}
	return "PASSWORD"
}

// NewRotationPassword returns 48 random hex characters, the shape the service
// controller generates.
func NewRotationPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// EngineRotation returns the command that sets the engine's password to next.
// The current password is used only by MySQL and MongoDB. MinIO returns nil
// because it takes its root password from the environment at start, and RabbitMQ returns nil because its password changes
// through ChangeRabbitMQPassword. Types without a password, an
// unsafe username, database name or password, and unknown types return an
// error.
func EngineRotation(svcType, username, database, current, next string) (*EngineCommand, error) {
	if !servicecatalog.HasAuth(svcType) {
		return nil, fmt.Errorf("a %s service runs without a password, so there is nothing to rotate", svcType)
	}
	if !validRotationPassword(next) {
		return nil, fmt.Errorf("the new password for %s is not one kip can pass to the engine safely", svcType)
	}
	if svcType == "minio" {
		return nil, nil
	}
	if !validServiceUsername(username) {
		return nil, fmt.Errorf("the service's username %q is not one kip can pass to the engine safely", username)
	}
	switch svcType {
	case "postgres":
		if err := validDatabaseName(database); err != nil {
			return nil, err
		}
		// \password prompts twice and reads stdin when there is no terminal. psql
		// hashes the password itself, so the server logs never see it.
		return &EngineCommand{
			Argv:  []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", username, "-d", database, "-c", `\password`},
			Stdin: next + "\n" + next + "\n",
		}, nil
	case "mysql":
		if strings.ContainsAny(current, "\r\n") {
			return nil, fmt.Errorf("the stored mysql password contains a line break, which kip cannot pass to mysql safely")
		}
		// The first line becomes MYSQL_PWD and the rest is the statement. The
		// image creates root@localhost, root@% and the service user, all with
		// this password.
		accounts := []string{"'root'@'localhost'", "'root'@'%'"}
		if username != "root" {
			accounts = append(accounts, "'"+username+"'@'%'")
		}
		for i, a := range accounts {
			accounts[i] = a + " IDENTIFIED BY '" + next + "'"
		}
		return &EngineCommand{
			Argv:  []string{"sh", "-c", `IFS= read -r p; MYSQL_PWD="$p" exec mysql -uroot --batch`},
			Stdin: current + "\n" + "ALTER USER IF EXISTS " + strings.Join(accounts, ", ") + ";\n",
		}, nil
	case "mongodb":
		// The pod's MONGO_INITDB_ROOT_PASSWORD can be older than the Secret, so
		// the script authenticates with the password it is given.
		user, _ := json.Marshal(username)
		cur, _ := json.Marshal(current)
		nxt, _ := json.Marshal(next)
		script := fmt.Sprintf(`const a = db.getSiblingDB("admin");
a.auth(%s, %s);
a.changeUserPassword(%s, %s);
`, user, cur, user, nxt)
		return &EngineCommand{
			Argv:  []string{"sh", "-c", `f=$(mktemp) && chmod 600 "$f" && cat >"$f" && mongosh --norc --quiet --file "$f" admin; rc=$?; rm -f "$f"; exit $rc`},
			Stdin: script,
		}, nil
	case "rabbitmq":
		return nil, nil
	}
	return nil, fmt.Errorf("kip cannot rotate the password of a %s service", svcType)
}

// RabbitMQManagementPort is where the management plugin in Kipper's RabbitMQ
// image listens.
const RabbitMQManagementPort = 15672

// ChangeRabbitMQPassword sets a RabbitMQ user's password through the
// management API at baseURL, signing in as that user with current. rabbitmqctl
// would start a second Erlang VM inside the broker's container, which a
// default-sized one has no memory for. The update replaces the user's tags, so
// they are read first and sent back unchanged. Request failures, unexpected HTTP
// statuses and invalid tag responses return errors.
func ChangeRabbitMQPassword(ctx context.Context, client *http.Client, baseURL, username, current, next string) error {
	userURL := strings.TrimSuffix(baseURL, "/") + "/api/users/" + url.PathEscape(username)
	call := func(method string, body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, userURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(username, current)
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	}

	resp, err := call(http.MethodGet, nil)
	if err != nil {
		return fmt.Errorf("reading RabbitMQ user %s: %w", username, err)
	}
	var user struct {
		Tags json.RawMessage `json:"tags"`
	}
	err = json.NewDecoder(resp.Body).Decode(&user)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("reading RabbitMQ user %s: the management API answered %s", username, resp.Status)
	}
	if err != nil {
		return fmt.Errorf("reading RabbitMQ user %s: %w", username, err)
	}
	tags, err := rabbitMQTags(user.Tags)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]string{"password": next, "tags": tags})
	if err != nil {
		return err
	}
	resp, err = call(http.MethodPut, body)
	if err != nil {
		return fmt.Errorf("changing the password of RabbitMQ user %s: %w", username, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("changing the password of RabbitMQ user %s: the management API answered %s", username, resp.Status)
	}
	return nil
}

// rabbitMQTags reads a user's tags, which RabbitMQ 3.9 and later return as a
// list and older releases as a comma-separated string, into the string form
// every release accepts on an update.
func rabbitMQTags(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, ","), nil
	}
	var joined string
	if err := json.Unmarshal(raw, &joined); err != nil {
		return "", fmt.Errorf("reading RabbitMQ user tags: %w", err)
	}
	return joined, nil
}

// validServiceUsername accepts the usernames console-api provisions
// databases with: 1 to 32 ASCII letters, digits, underscores, dashes or dots.
func validServiceUsername(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// validRotationPassword accepts 16 to 128 ASCII letters and digits. The new
// password reaches SQL and shell input, and the record holding it is a Secret
// anyone with Secret write access in the namespace could have written.
func validRotationPassword(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

const (
	annoRotationHolder  = "kipper.run/rotation-holder"
	annoRotationExpires = "kipper.run/rotation-expires"

	// RotationLease is how long a claim keeps other runs away from a rotation.
	// It comfortably covers the engine change and the commit that follows.
	RotationLease = 5 * time.Minute
)

// Rotation records a password rotation in a Secret owned by the Service and
// leases it to one run at a time. Password is the target password. When Resumed
// is true, an earlier run created the record and may already have changed the engine.
type Rotation struct {
	clientset kubernetes.Interface
	namespace string
	service   string
	record    string
	holder    string
	now       func() time.Time

	Password string
	Resumed  bool
}

// ClaimRotation leases the rotation of the owner's password to this run. It
// creates the record with a new password, or takes over the record an earlier
// run left once that run's lease has run out. A lease another run still holds
// is an error naming when it ends. Invalid stored passwords also return an error.
// Creation and takeover conflicts are retried up to five times. The now function
// supplies the clock for this claim and all later lease checks.
func ClaimRotation(ctx context.Context, clientset kubernetes.Interface, namespace string, owner metav1.OwnerReference, now func() time.Time) (*Rotation, error) {
	holder, err := NewRotationPassword()
	if err != nil {
		return nil, err
	}
	r := &Rotation{clientset: clientset, namespace: namespace, service: owner.Name, record: secretname.ServicePasswordRotation(owner.Name), holder: holder, now: now}
	secrets := clientset.CoreV1().Secrets(namespace)
	lease := r.lease()

	for range 5 {
		existing, err := secrets.Get(ctx, r.record, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if r.Password, err = NewRotationPassword(); err != nil {
				return nil, err
			}
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: r.record, Namespace: namespace, Annotations: lease, OwnerReferences: []metav1.OwnerReference{owner}},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{"PASSWORD": []byte(r.Password)},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("recording the rotation of %s: %w", owner.Name, err)
			}
			return r, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", r.record, err)
		}
		if until, held := leaseHeld(existing, now()); held {
			return nil, fmt.Errorf("another run is rotating the password of %s and holds it until %s", owner.Name, until.UTC().Format(time.RFC3339))
		}
		r.Password = string(existing.Data["PASSWORD"])
		if !validRotationPassword(r.Password) {
			return nil, fmt.Errorf("%s holds a password outside the format kip accepts, so kip will not use it. kip service credentials --repair removes it", r.record)
		}
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		maps.Copy(existing.Annotations, lease)
		existing.OwnerReferences = []metav1.OwnerReference{owner}
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("taking over the rotation of %s: %w", owner.Name, err)
		}
		r.Resumed = true
		return r, nil
	}
	return nil, fmt.Errorf("could not claim the rotation of %s: %s kept changing", owner.Name, r.record)
}

// leaseHeld reports whether a run holds the record's lease at now, and until
// when. An unreadable expiry counts as expired.
func leaseHeld(record *corev1.Secret, now time.Time) (time.Time, bool) {
	if record.Annotations[annoRotationHolder] == "" {
		return time.Time{}, false
	}
	until, err := time.Parse(time.RFC3339, record.Annotations[annoRotationExpires])
	if err != nil {
		return time.Time{}, false
	}
	return until, now.Before(until)
}

func (r *Rotation) lease() map[string]string {
	return map[string]string{annoRotationHolder: r.holder, annoRotationExpires: r.now().Add(RotationLease).UTC().Format(time.RFC3339)}
}

// held returns the record while this run still holds an unexpired lease on it.
func (r *Rotation) held(ctx context.Context) (*corev1.Secret, error) {
	record, err := r.clientset.CoreV1().Secrets(r.namespace).Get(ctx, r.record, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("another run finished the rotation of %s", r.service)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", r.record, err)
	}
	if record.Annotations[annoRotationHolder] != r.holder {
		return nil, fmt.Errorf("another run took over the rotation of %s", r.service)
	}
	if _, ok := leaseHeld(record, r.now()); !ok {
		return nil, fmt.Errorf("this run's hold on the rotation of %s ran out", r.service)
	}
	return record, nil
}

// Extend renews this run's unexpired lease for a full RotationLease before
// commit, so a suspended run stops if its lease expired or another run took over.
// Conflicting updates are retried after rechecking the lease.
func (r *Rotation) Extend(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		record, err := r.held(ctx)
		if err != nil {
			return err
		}
		maps.Copy(record.Annotations, r.lease())
		_, err = r.clientset.CoreV1().Secrets(r.namespace).Update(ctx, record, metav1.UpdateOptions{})
		return err
	})
}

// Commit writes Password into key of the credentials Secret. It checks before
// every attempt that this run holds an unexpired lease and returns an error
// if the check fails. Conflicting writes are retried. The lease check and the
// credentials write are separate API requests, so they are not atomic.
func (r *Rotation) Commit(ctx context.Context, credentialsSecret, key string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if _, err := r.held(ctx); err != nil {
			return err
		}
		secret, err := r.clientset.CoreV1().Secrets(r.namespace).Get(ctx, credentialsSecret, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[key] = []byte(r.Password)
		_, err = r.clientset.CoreV1().Secrets(r.namespace).Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("writing the new password to %s: %w", credentialsSecret, err)
	}
	return nil
}

// Finish deletes the rotation record while this run holds an unexpired lease.
// The caller must commit first. Conflicts are retried after rechecking the lease,
// and a missing record or lost lease at that check returns an error.
func (r *Rotation) Finish(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		record, err := r.held(ctx)
		if err != nil {
			return err
		}
		err = r.clientset.CoreV1().Secrets(r.namespace).Delete(ctx, r.record, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion},
		})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

// Release gives up this run's lease and keeps the record, so the next run can
// finish the rotation right away. It fails when this run no longer holds
// the lease, which leaves nothing to give up.
func (r *Rotation) Release(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		record, err := r.held(ctx)
		if err != nil {
			return err
		}
		delete(record.Annotations, annoRotationHolder)
		delete(record.Annotations, annoRotationExpires)
		_, err = r.clientset.CoreV1().Secrets(r.namespace).Update(ctx, record, metav1.UpdateOptions{})
		return err
	})
}

// RotationRecord describes a service's rotation record. RotationNone means the
// record is absent. RotationUnfinished means it holds a usable password and may
// still be in use. RotationUnusable means its password fails validation and
// blocks rotation until the record is removed.
type RotationRecord string

const (
	RotationNone       RotationRecord = ""
	RotationUnfinished RotationRecord = "unfinished"
	RotationUnusable   RotationRecord = "unusable"
)

// ReadRotationRecord classifies the service's rotation record by its password.
// It returns RotationNone for a missing record and propagates other read errors.
// It does not check whether a run holds the lease.
func ReadRotationRecord(ctx context.Context, clientset kubernetes.Interface, namespace, service string) (RotationRecord, error) {
	record, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secretname.ServicePasswordRotation(service), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return RotationNone, nil
	case err != nil:
		return RotationNone, err
	case !validRotationPassword(string(record.Data["PASSWORD"])):
		return RotationUnusable, nil
	}
	return RotationUnfinished, nil
}

// RemoveUnusableRotationRecord deletes the service's rotation record if it
// still holds a password kip refuses to use. It returns true only when this
// call deletes the record. Missing or usable records return false with no error.
// A concurrent change prevents deletion and returns an error.
func RemoveUnusableRotationRecord(ctx context.Context, clientset kubernetes.Interface, namespace, service string) (bool, error) {
	name := secretname.ServicePasswordRotation(service)
	record, err := clientset.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", name, err)
	}
	if validRotationPassword(string(record.Data["PASSWORD"])) {
		return false, nil
	}
	err = clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("removing %s: %w", name, err)
	}
	return err == nil, nil
}

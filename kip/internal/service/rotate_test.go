package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	currentSentinel = "0123456789abcdef0123456789abcdef0123456789abcdef" //nolint:gosec // a test value, not a credential
	nextSentinel    = "fedcba9876543210fedcba9876543210fedcba9876543210" //nolint:gosec // a test value, not a credential
)

func TestEveryEngineGetsThePasswordsOnStdinOnly(t *testing.T) {
	for _, svcType := range []string{"postgres", "mysql", "mongodb"} {
		t.Run(svcType, func(t *testing.T) {
			cmd, err := EngineRotation(svcType, "kipper", "app", currentSentinel, nextSentinel)
			require.NoError(t, err)
			require.NotNil(t, cmd)

			argv := strings.Join(cmd.Argv, " ")
			assert.NotContains(t, argv, currentSentinel)
			assert.NotContains(t, argv, nextSentinel)
			assert.Contains(t, cmd.Stdin, nextSentinel)
		})
	}
}

func TestPostgresChangesThePasswordWithPsqlsOwnPrompt(t *testing.T) {
	cmd, err := EngineRotation("postgres", "kipper", "app", currentSentinel, nextSentinel)
	require.NoError(t, err)

	assert.Equal(t, []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "kipper", "-d", "app", "-c", `\password`}, cmd.Argv)
	assert.Equal(t, nextSentinel+"\n"+nextSentinel+"\n", cmd.Stdin)
	assert.NotContains(t, cmd.Stdin, currentSentinel, "the local socket needs no password")
}

func TestMySQLChangesRootAndTheServiceUser(t *testing.T) {
	cmd, err := EngineRotation("mysql", "kipper", "app", currentSentinel, nextSentinel)
	require.NoError(t, err)

	lines := strings.SplitN(cmd.Stdin, "\n", 2)
	assert.Equal(t, currentSentinel, lines[0], "the current password comes first, for MYSQL_PWD")
	assert.Equal(t, "ALTER USER IF EXISTS 'root'@'localhost' IDENTIFIED BY '"+nextSentinel+"', 'root'@'%' IDENTIFIED BY '"+nextSentinel+
		"', 'kipper'@'%' IDENTIFIED BY '"+nextSentinel+"';\n", lines[1])
}

func TestMySQLDoesNotNameRootTwice(t *testing.T) {
	cmd, err := EngineRotation("mysql", "root", "app", currentSentinel, nextSentinel)
	require.NoError(t, err)

	assert.Equal(t, 2, strings.Count(cmd.Stdin, "IDENTIFIED BY"))
}

func TestMongoDBAuthenticatesWithTheGivenPassword(t *testing.T) {
	cmd, err := EngineRotation("mongodb", "kipper", "app", `cur"rent\`, nextSentinel)
	require.NoError(t, err)

	user, _ := json.Marshal("kipper")
	current, _ := json.Marshal(`cur"rent\`)
	next, _ := json.Marshal(nextSentinel)
	assert.Contains(t, cmd.Stdin, "a.auth("+string(user)+", "+string(current)+")")
	assert.Contains(t, cmd.Stdin, "a.changeUserPassword("+string(user)+", "+string(next)+")")
	assert.NotContains(t, strings.Join(cmd.Argv, " "), "MONGO_INITDB_ROOT_PASSWORD", "the pod's env can be older than the Secret")
}

func TestRabbitMQChangesThePasswordThroughTheManagementAPI(t *testing.T) {
	for _, tags := range []string{`["administrator","monitoring"]`, `"administrator,monitoring"`} {
		var put map[string]string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, _ := r.BasicAuth()
			if user != "kipper" || pass != currentSentinel {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			assert.Equal(t, "/api/users/kipper", r.URL.Path)
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"name":"kipper","tags":` + tags + `}`))
			case http.MethodPut:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&put))
				w.WriteHeader(http.StatusNoContent)
			}
		}))

		require.NoError(t, ChangeRabbitMQPassword(context.Background(), srv.Client(), srv.URL, "kipper", currentSentinel, nextSentinel))
		srv.Close()

		assert.Equal(t, map[string]string{"password": nextSentinel, "tags": "administrator,monitoring"}, put, "the update must keep the tags it replaces")
	}
}

func TestRabbitMQRefusesTheWrongCurrentPasswordWithoutLeakingIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method, "nothing is written after a failed sign-in")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not_authorised"}`))
	}))
	defer srv.Close()

	err := ChangeRabbitMQPassword(context.Background(), srv.Client(), srv.URL, "kipper", currentSentinel, nextSentinel)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.NotContains(t, err.Error(), currentSentinel)
	assert.NotContains(t, err.Error(), nextSentinel)
}

func TestRabbitMQHasNoExecStep(t *testing.T) {
	cmd, err := EngineRotation("rabbitmq", "kipper", "", currentSentinel, nextSentinel)

	require.NoError(t, err)
	assert.Nil(t, cmd, "rabbitmqctl would start a second Erlang VM in the broker's container")
	_, err = EngineRotation("rabbitmq", "kip per", "", currentSentinel, nextSentinel)
	assert.Error(t, err, "the username still has to be one kip can pass safely")
}

func TestMinIOHasNoEngineStep(t *testing.T) {
	cmd, err := EngineRotation("minio", "kipper", "", currentSentinel, nextSentinel)

	require.NoError(t, err)
	assert.Nil(t, cmd)
	assert.Equal(t, "SECRET_KEY", RotatedKey("minio"))
	assert.Equal(t, "PASSWORD", RotatedKey("postgres"))
}

func TestMinIOIsCheckedByItsOwnKeysRatherThanAUsername(t *testing.T) {
	cmd, err := EngineRotation("minio", "", "", currentSentinel, nextSentinel)

	require.NoError(t, err, "MinIO credentials carry ACCESS_KEY and SECRET_KEY, no USERNAME")
	assert.Nil(t, cmd)
}

func TestRotationRefusesAPasswordThatCouldInjectIntoTheEngine(t *testing.T) {
	for _, next := range []string{"", "short", "abc'; DROP USER root; --abcdef", strings.Repeat("a", 129), "line\nbreak0123456789"} {
		_, err := EngineRotation("mysql", "kipper", "app", currentSentinel, next)
		assert.Error(t, err, "next %q", next)
	}
	_, err := EngineRotation("mysql", "kipper", "app", "two\nlines", nextSentinel)
	assert.Error(t, err, "the stored password's first line becomes MYSQL_PWD")
}

func TestRotationRefusesWhatItCannotRotateSafely(t *testing.T) {
	for _, svcType := range []string{"redis", "opensearch", "mailhog", "something"} {
		_, err := EngineRotation(svcType, "kipper", "app", currentSentinel, nextSentinel)
		require.Error(t, err, svcType)
		assert.Contains(t, err.Error(), svcType)
	}
	for _, user := range []string{"", "a'b", "kip per", strings.Repeat("a", 33)} {
		_, err := EngineRotation("mysql", user, "app", currentSentinel, nextSentinel)
		assert.Error(t, err, "username %q", user)
	}
	_, err := EngineRotation("postgres", "kipper", "app;drop", currentSentinel, nextSentinel)
	assert.Error(t, err, "the database name reaches psql's argv")
}

func TestNewRotationPasswordIsLongHex(t *testing.T) {
	a, err := NewRotationPassword()
	require.NoError(t, err)
	b, err := NewRotationPassword()
	require.NoError(t, err)

	assert.Regexp(t, `^[0-9a-f]{48}$`, a)
	assert.NotEqual(t, a, b)
}

var (
	rotationNow = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	dbOwner     = metav1.OwnerReference{APIVersion: "kipper.run/v1", Kind: "Service", Name: "db", UID: "uid-db"}
)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

func dbCredentials() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop", Annotations: map[string]string{"other": "kept"}},
		Data:       map[string][]byte{"USERNAME": []byte("kipper"), "PASSWORD": []byte(currentSentinel)},
	}
}

func record(t *testing.T, cs *fake.Clientset) *corev1.Secret {
	t.Helper()
	s, err := cs.CoreV1().Secrets("shop").Get(context.Background(), "db-password-rotation", metav1.GetOptions{})
	require.NoError(t, err)
	return s
}

func TestAClaimRecordsTheNewPasswordInSecretDataOnly(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())

	r, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))
	require.NoError(t, err)

	assert.False(t, r.Resumed)
	assert.Regexp(t, `^[0-9a-f]{48}$`, r.Password)
	rec := record(t, cs)
	assert.Equal(t, r.Password, string(rec.Data["PASSWORD"]))
	for _, v := range rec.Annotations {
		assert.NotContains(t, v, r.Password, "kubectl describe prints annotations")
	}
	assert.Equal(t, []metav1.OwnerReference{dbOwner}, rec.OwnerReferences)
	creds, _ := cs.CoreV1().Secrets("shop").Get(context.Background(), "db-credentials", metav1.GetOptions{})
	assert.Equal(t, currentSentinel, string(creds.Data["PASSWORD"]), "bindings see nothing until the commit")
}

func TestASecondRunIsRefusedWhileTheFirstHoldsTheLease(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	_, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))
	require.NoError(t, err)

	_, err = ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow.Add(RotationLease-time.Second)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "another run is rotating")
}

func TestARunTakesOverAnExpiredLeaseWithTheSamePassword(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	first, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))
	require.NoError(t, err)

	second, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow.Add(RotationLease)))
	require.NoError(t, err)

	assert.True(t, second.Resumed)
	assert.Equal(t, first.Password, second.Password)
	require.Error(t, first.Commit(context.Background(), "db-credentials", "PASSWORD"), "the run that lost the lease must not write")
	require.Error(t, first.Finish(context.Background()))
	assert.Equal(t, second.Password, string(record(t, cs).Data["PASSWORD"]))
}

func TestAReleasedRotationCanBeResumedAtOnce(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	first, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))
	require.NoError(t, err)

	require.NoError(t, first.Release(context.Background()))
	second, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow.Add(time.Second)))

	require.NoError(t, err)
	assert.True(t, second.Resumed)
	assert.Equal(t, first.Password, second.Password, "the engine may already hold it")
}

func TestATakeoverRefusesARecordKipWouldNotHaveWritten(t *testing.T) {
	planted := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-password-rotation", Namespace: "shop"},
		Data:       map[string][]byte{"PASSWORD": []byte("x' IDENTIFIED BY 'y")},
	}
	cs := fake.NewSimpleClientset(dbCredentials(), planted)

	_, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))

	assert.Error(t, err)
}

func TestAClaimRetriesWhenAnotherRunCreatesTheRecordFirst(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	raced := false
	cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if raced {
			return false, nil, nil
		}
		raced = true
		// The other run's record lands between this run's read and its create.
		// The tracker is written directly because the fake's own client would
		// wait on the lock this reactor runs under.
		require.NoError(t, cs.Tracker().Add(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-password-rotation", Namespace: "shop", Annotations: map[string]string{
				annoRotationHolder: "other-run", annoRotationExpires: rotationNow.Add(RotationLease).Format(time.RFC3339),
			}},
			Data: map[string][]byte{"PASSWORD": []byte(nextSentinel)},
		}))
		return true, nil, apierrors.NewAlreadyExists(corev1.Resource("secrets"), "db-password-rotation")
	})

	_, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))

	require.Error(t, err, "the other run now holds the lease")
	assert.Contains(t, err.Error(), "another run is rotating")
}

func TestCommitWritesTheKeyAndFinishDeletesTheRecord(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	r, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))
	require.NoError(t, err)

	require.NoError(t, r.Commit(context.Background(), "db-credentials", "PASSWORD"))
	state, err := ReadRotationRecord(context.Background(), cs, "shop", "db")
	require.NoError(t, err)
	assert.Equal(t, RotationUnfinished, state, "the record stays until Finish")
	require.NoError(t, r.Finish(context.Background()))

	s, _ := cs.CoreV1().Secrets("shop").Get(context.Background(), "db-credentials", metav1.GetOptions{})
	assert.Equal(t, r.Password, string(s.Data["PASSWORD"]))
	assert.Equal(t, "kipper", string(s.Data["USERNAME"]))
	assert.Equal(t, "kept", s.Annotations["other"])
	state, err = ReadRotationRecord(context.Background(), cs, "shop", "db")
	require.NoError(t, err)
	assert.Equal(t, RotationNone, state)
}

func TestCommitRechecksTheLeaseOnAConflict(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	r, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, at(rotationNow))
	require.NoError(t, err)
	conflicts := 1
	cs.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 || a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret).Name != "db-credentials" {
			return false, nil, nil
		}
		conflicts--
		// Another run takes the rotation over while this one retries.
		obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), "shop", "db-password-rotation")
		require.NoError(t, err)
		record := obj.(*corev1.Secret).DeepCopy()
		record.Annotations[annoRotationHolder] = "other-run"
		require.NoError(t, cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), record, "shop"))
		return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), "db-credentials", errors.New("changed"))
	})

	err = r.Commit(context.Background(), "db-credentials", "PASSWORD")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "took over")
	s, _ := cs.CoreV1().Secrets("shop").Get(context.Background(), "db-credentials", metav1.GetOptions{})
	assert.Equal(t, currentSentinel, string(s.Data["PASSWORD"]))
}

func TestARunPastItsLeaseCannotChangeAnythingAndExtendRenewsIt(t *testing.T) {
	cs := fake.NewSimpleClientset(dbCredentials())
	now := rotationNow
	r, err := ClaimRotation(context.Background(), cs, "shop", dbOwner, func() time.Time { return now })
	require.NoError(t, err)

	now = now.Add(RotationLease - time.Minute)
	require.NoError(t, r.Extend(context.Background()))
	now = now.Add(RotationLease - time.Minute)
	require.NoError(t, r.Extend(context.Background()), "the renewal counts from the extension, not the claim")

	now = now.Add(RotationLease)
	assert.Error(t, r.Extend(context.Background()), "a suspended run stops before the engine")
	assert.Error(t, r.Commit(context.Background(), "db-credentials", "PASSWORD"))
	s, _ := cs.CoreV1().Secrets("shop").Get(context.Background(), "db-credentials", metav1.GetOptions{})
	assert.Equal(t, currentSentinel, string(s.Data["PASSWORD"]))
}

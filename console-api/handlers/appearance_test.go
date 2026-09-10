package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func appearanceConfigMapWith(colour string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: appearanceConfigMap, Namespace: appearanceNamespace},
		Data:       map[string]string{faviconColourKey: colour},
	}
}

func readFaviconColour(t *testing.T, body *bytes.Buffer) string {
	t.Helper()
	var got appearanceSettings
	require.NoError(t, json.Unmarshal(body.Bytes(), &got))
	return got.FaviconColour
}

func TestAppearanceGetFallsBackToBlue(t *testing.T) {
	cases := []struct {
		name   string
		stored *corev1.ConfigMap
		want   string
	}{
		{"no settings have ever been saved", nil, "blue"},
		{"a colour this build can draw", appearanceConfigMapWith("green"), "green"},
		{"a colour this build does not know", appearanceConfigMapWith("chartreuse"), "blue"},
		{"the key is present but empty", appearanceConfigMapWith(""), "blue"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			if tc.stored != nil {
				client = fake.NewClientset(tc.stored)
			}
			h := &Appearance{Client: client}

			rec := httptest.NewRecorder()
			h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.want, readFaviconColour(t, rec.Body))
		})
	}
}

func TestAppearanceUpdateSavesTheColour(t *testing.T) {
	client := fake.NewClientset()
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"faviconColour":"purple"}`)
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", body))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "purple", readFaviconColour(t, rec.Body))

	cm, err := client.CoreV1().ConfigMaps(appearanceNamespace).Get(context.Background(), appearanceConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "purple", cm.Data[faviconColourKey])
}

func TestAppearanceUpdateReplacesAColourAlreadySet(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("red"))
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"faviconColour":"grey"}`)
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", body))

	require.Equal(t, http.StatusOK, rec.Code)
	cm, err := client.CoreV1().ConfigMaps(appearanceNamespace).Get(context.Background(), appearanceConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "grey", cm.Data[faviconColourKey])
}

func TestAppearanceUpdateRefusesAColourTheConsoleCannotDraw(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("green"))
	h := &Appearance{Client: client}

	for _, colour := range []string{"chartreuse", "white", "", "BLUE", "#0EA5E9"} {
		t.Run("refuses "+colour, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := bytes.NewBufferString(`{"faviconColour":"` + colour + `"}`)
			h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", body))

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			cm, err := client.CoreV1().ConfigMaps(appearanceNamespace).Get(context.Background(), appearanceConfigMap, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "green", cm.Data[faviconColourKey], "a rejected colour must leave the stored one alone")
		})
	}
}

func TestAppearanceUpdateRefusesAnUnreadableBody(t *testing.T) {
	h := &Appearance{Client: fake.NewClientset()}

	rec := httptest.NewRecorder()
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", bytes.NewBufferString(`not json`)))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEveryOfferedColourIsAccepted(t *testing.T) {
	for colour := range faviconColours {
		t.Run(colour, func(t *testing.T) {
			client := fake.NewClientset()
			h := &Appearance{Client: client}

			rec := httptest.NewRecorder()
			body := bytes.NewBufferString(`{"faviconColour":"` + colour + `"}`)
			h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", body))
			require.Equal(t, http.StatusOK, rec.Code)

			h.nextRefresh = time.Time{} // read it back from the cluster, not the cache
			getRec := httptest.NewRecorder()
			h.Get(getRec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
			assert.Equal(t, colour, readFaviconColour(t, getRec.Body), "a saved colour must read back")
		})
	}
}

// rejectUpdatesWithoutResourceVersion makes the fake client refuse what a real
// API server refuses. Without it a handler that posts a freshly built object
// passes every test here and fails on the first save against a cluster.
func rejectUpdatesWithoutResourceVersion(client *fake.Clientset) {
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		cm, ok := action.(k8stesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if ok && cm.ResourceVersion == "" {
			return true, nil, apierrors.NewInvalid(
				schema.GroupKind{Kind: "ConfigMap"}, cm.Name, nil)
		}
		return false, nil, nil
	})
}

func TestAppearanceUpdateCarriesTheResourceVersionAnAPIServerDemands(t *testing.T) {
	existing := appearanceConfigMapWith("red")
	existing.ResourceVersion = "42"
	client := fake.NewClientset(existing)
	rejectUpdatesWithoutResourceVersion(client)
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", bytes.NewBufferString(`{"faviconColour":"green"}`)))

	require.Equal(t, http.StatusOK, rec.Code, "the update must be shaped the way a cluster accepts")
	cm, err := client.CoreV1().ConfigMaps(appearanceNamespace).Get(context.Background(), appearanceConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "green", cm.Data[faviconColourKey])
}

func TestAppearanceUpdateKeepsKeysItDoesNotOwn(t *testing.T) {
	existing := appearanceConfigMapWith("red")
	existing.ResourceVersion = "7"
	existing.Data["somethingElse"] = "left alone"
	client := fake.NewClientset(existing)
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", bytes.NewBufferString(`{"faviconColour":"pink"}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	cm, err := client.CoreV1().ConfigMaps(appearanceNamespace).Get(context.Background(), appearanceConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "pink", cm.Data[faviconColourKey])
	assert.Equal(t, "left alone", cm.Data["somethingElse"])
}

func TestAppearanceUpdateSurvivesTwoAdminsCreatingAtOnce(t *testing.T) {
	client := fake.NewClientset()
	h := &Appearance{Client: client}

	// The first create loses the race: somebody else made the object between
	// this handler's read and its write.
	var creates int
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		creates++
		if creates == 1 {
			cm := appearanceConfigMapWith("orange")
			cm.ResourceVersion = "1"
			_ = client.Tracker().Add(cm)
			return true, nil, apierrors.NewAlreadyExists(
				schema.GroupResource{Resource: "configmaps"}, appearanceConfigMap)
		}
		return false, nil, nil
	})

	rec := httptest.NewRecorder()
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", bytes.NewBufferString(`{"faviconColour":"grey"}`)))

	require.Equal(t, http.StatusOK, rec.Code, "losing the create race must not fail the save")
	cm, err := client.CoreV1().ConfigMaps(appearanceNamespace).Get(context.Background(), appearanceConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "grey", cm.Data[faviconColourKey])
}

func TestAppearanceUpdateRetriesAConflict(t *testing.T) {
	existing := appearanceConfigMapWith("red")
	existing.ResourceVersion = "3"
	client := fake.NewClientset(existing)
	h := &Appearance{Client: client}

	var updates, gets int
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, appearanceConfigMap, nil)
		}
		return false, nil, nil
	})

	rec := httptest.NewRecorder()
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", bytes.NewBufferString(`{"faviconColour":"brown"}`)))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Greater(t, updates, 1, "a conflict must be retried, not returned")
	assert.Greater(t, gets, 1, "each attempt must read the object back, or it would write a stale one")
}

func TestAppearanceGetDoesNotAskTheClusterEveryTime(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("yellow"))
	var gets int
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return false, nil, nil
	})
	h := &Appearance{Client: client}

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
		require.Equal(t, "yellow", readFaviconColour(t, rec.Body))
	}

	assert.Equal(t, 1, gets, "an unauthenticated read must not become one API call per request")
}

func TestAppearanceGetKeepsTheLastAnswerWhenTheClusterGoesAway(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("purple"))
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
	require.Equal(t, "purple", readFaviconColour(t, rec.Body))

	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the API server is down")
	})
	h.nextRefresh = time.Time{} // force a refresh, which will now fail

	rec = httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
	assert.Equal(t, "purple", readFaviconColour(t, rec.Body), "an unreachable cluster must not flip every tab back to blue")
}

func TestAppearanceUpdateIsVisibleToTheNextRead(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("blue"))
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
	require.Equal(t, "blue", readFaviconColour(t, rec.Body))

	rec = httptest.NewRecorder()
	h.Update(rec, httptest.NewRequest(http.MethodPut, "/api/v1/settings/appearance", bytes.NewBufferString(`{"faviconColour":"green"}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
	assert.Equal(t, "green", readFaviconColour(t, rec.Body), "a save must not be hidden behind the read cache")
}

func TestAppearanceGetCoalescesAConcurrentColdStart(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("orange"))
	release := make(chan struct{})
	var gets int32
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&gets, 1)
		<-release // hold the first caller inside the refresh while the rest pile up
		return false, nil, nil
	})
	h := &Appearance{Client: client}

	var wg sync.WaitGroup
	answers := make([]string, 20)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
			answers[i] = readFaviconColour(t, rec.Body)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&gets), "a cold burst must cost one API call, not one per caller")
	for _, answer := range answers {
		assert.Equal(t, "orange", answer)
	}
}

func TestAppearanceGetHoldsOffAfterAFailedRefresh(t *testing.T) {
	client := fake.NewClientset(appearanceConfigMapWith("green"))
	h := &Appearance{Client: client}

	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
	require.Equal(t, "green", readFaviconColour(t, rec.Body))

	var gets int
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return true, nil, apierrors.NewServiceUnavailable("the API server is down")
	})
	h.nextRefresh = time.Time{}

	for i := 0; i < 10; i++ {
		rec = httptest.NewRecorder()
		h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/appearance", nil))
		require.Equal(t, "green", readFaviconColour(t, rec.Body))
	}

	assert.Equal(t, 1, gets, "an outage must not cost one failed API call per reader")
}

package handlers

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

// dexConnectorSecretName is the Secret in the dex namespace that holds the
// SSO client secrets. Dex loads them as environment variables referenced
// by its connector configuration.
const dexConnectorSecretName = "dex-connector-secrets" //nolint:gosec // G101: Kubernetes Secret object name, not a credential value

// AuthConnectors handles OAuth connector configuration.
type AuthConnectors struct {
	Client kubernetes.Interface
}

type connectorConfig struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Org          string `json:"org,omitempty"`
	Domain       string `json:"domain,omitempty"`
	BaseURL      string `json:"base_url,omitempty"`
	Enabled      bool   `json:"enabled"`
}

type connectorResponse struct {
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	HasKeys bool   `json:"has_keys"`
}

// List returns the current connector configurations.
// GET /api/v1/settings/auth
func (ac *AuthConnectors) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	config, err := ac.loadDexConfig(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read auth config")
		return
	}

	connectors, _ := config["connectors"].([]interface{})

	result := []connectorResponse{
		{Type: "github", Enabled: false},
		{Type: "gitlab", Enabled: false},
		{Type: "google", Enabled: false},
	}

	for _, c := range connectors {
		conn, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		connType, _ := conn["type"].(string)
		for i := range result {
			if result[i].Type == connType {
				result[i].Enabled = true
				result[i].HasKeys = true
			}
		}
	}

	respondJSON(w, http.StatusOK, result)
}

// Update configures an OAuth connector.
// PUT /api/v1/settings/auth
func (ac *AuthConnectors) Update(w http.ResponseWriter, r *http.Request) {
	var req connectorConfig
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Type != "github" && req.Type != "gitlab" && req.Type != "google" {
		respondError(w, http.StatusBadRequest, "type must be github, gitlab, or google")
		return
	}
	enable := req.Enabled && req.ClientID != ""
	if enable && !dexCanExpand(req.ClientSecret) {
		respondError(w, http.StatusBadRequest, "client_secret must not contain quotes, backslashes or control characters")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if enable {
		// A unique key preserves the client ID and secret pair used by an existing
		// config if this update fails or overlaps another update.
		env := connectorSecretEnv(req.Type)
		// Store the secret and configure Dex to load it before saving its reference,
		// so a restart between these steps can still load the current config.
		if err := ac.storeConnectorSecrets(ctx, map[string]string{env: req.ClientSecret}); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %v", err))
			return
		}
		if err := ac.ensureDexReadsConnectorSecrets(ctx); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %v", err))
			return
		}
		req.ClientSecret = "$" + env
	}

	err := ac.updateDexConfig(ctx, func(config map[string]interface{}) (bool, error) {
		connectors, _ := config["connectors"].([]interface{})
		var filtered []interface{}
		for _, c := range connectors {
			if conn, ok := c.(map[string]interface{}); ok {
				if t, _ := conn["type"].(string); t == req.Type {
					continue
				}
			}
			filtered = append(filtered, c)
		}
		if enable {
			filtered = append(filtered, ac.buildConnector(req, config))
		}
		config["connectors"] = filtered
		return true, nil
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %v", err))
		return
	}

	if err := ac.restartDex(ctx); err != nil {
		log.Printf("auth connectors: restarting Dex: %v", err)
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// MoveDexConnectorSecrets moves inline client secrets into dex-connector-secrets,
// replaces them with environment variable references, and requests a Dex restart.
// Secrets with characters rejected by dexCanExpand remain inline. If no eligible
// secrets exist, it returns without writing. Errors stop the migration, but earlier
// writes remain committed.
func MoveDexConnectorSecrets(ctx context.Context, client kubernetes.Interface) error {
	ac := &AuthConnectors{Client: client}
	// Reuse keys for each connector and secret across conflict retries to avoid
	// accumulating duplicate entries in the Secret.
	envs := map[string]string{}
	moved := false
	err := ac.updateDexConfig(ctx, func(config map[string]interface{}) (bool, error) {
		connectors, _ := config["connectors"].([]interface{})
		values := map[string]string{}
		for _, c := range connectors {
			conn, _ := c.(map[string]interface{})
			connConfig, _ := conn["config"].(map[string]interface{})
			id, _ := conn["id"].(string)
			secret, _ := connConfig["clientSecret"].(string)
			if id == "" || secret == "" || strings.HasPrefix(secret, "$") {
				continue
			}
			if !dexCanExpand(secret) {
				log.Printf("auth connectors: the client secret of connector %s has characters Dex cannot expand, so it stays in the Dex config", id)
				continue
			}
			env, ok := envs[id+"\x00"+secret]
			if !ok {
				env = connectorSecretEnv(id)
				envs[id+"\x00"+secret] = env
			}
			values[env] = secret
			connConfig["clientSecret"] = "$" + env
		}
		moved = len(values) > 0
		if !moved {
			return false, nil
		}
		if err := ac.storeConnectorSecrets(ctx, values); err != nil {
			return false, err
		}
		return true, ac.ensureDexReadsConnectorSecrets(ctx)
	})
	if err != nil || !moved {
		return err
	}
	return ac.restartDex(ctx)
}

// connectorSecretEnv names a new environment variable for a connector's client
// secret. A random suffix protects references in saved configs and concurrent
// updates from having their values replaced.
func connectorSecretEnv(connectorID string) string {
	name := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToUpper(connectorID))
	return "KIPPER_" + name + "_CLIENT_SECRET_" + rand.Text()[:12]
}

// dexCanExpand reports whether Dex can expand secret into its connector
// config. Expansion happens inside JSON text, so quotes, backslashes, and
// control characters could change the value or invalidate the config.
func dexCanExpand(secret string) bool {
	return !strings.ContainsFunc(secret, func(r rune) bool {
		return r == '"' || r == '\\' || r < 0x20 || r == 0x7f
	})
}

// storeConnectorSecrets adds values to the dex-connector-secrets Secret,
// creating it if needed. Conflicts trigger a fresh read to preserve concurrent
// writes to other keys.
func (ac *AuthConnectors) storeConnectorSecrets(ctx context.Context, values map[string]string) error {
	secrets := ac.Client.CoreV1().Secrets("dex")
	raced := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	return retry.OnError(retry.DefaultRetry, raced, func() error {
		secret, err := secrets.Get(ctx, dexConnectorSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: dexConnectorSecretName, Namespace: "dex"}}
			for k, v := range values {
				setSecretValue(secret, k, v)
			}
			_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		for k, v := range values {
			setSecretValue(secret, k, v)
		}
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
}

func setSecretValue(secret *corev1.Secret, key, value string) {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[key] = []byte(value)
}

// ensureDexReadsConnectorSecrets loads dex-connector-secrets into the Dex
// container's environment if needed, triggering a rollout. The added reference
// is optional so Dex can start before the Secret exists.
func (ac *AuthConnectors) ensureDexReadsConnectorSecrets(ctx context.Context) error {
	deploys := ac.Client.AppsV1().Deployments("dex")
	deploy, err := deploys.Get(ctx, "dex", metav1.GetOptions{})
	if err != nil {
		return err
	}
	containers := deploy.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name != "dex" {
			continue
		}
		for _, from := range containers[i].EnvFrom {
			if from.SecretRef != nil && from.SecretRef.Name == dexConnectorSecretName {
				return nil
			}
		}
		optional := true
		containers[i].EnvFrom = append(containers[i].EnvFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: dexConnectorSecretName},
			Optional:             &optional,
		}})
		_, err = deploys.Update(ctx, deploy, metav1.UpdateOptions{})
		return err
	}
	return fmt.Errorf("the dex Deployment has no container named dex")
}

// restartDex rolls Dex, which reads its config only when it starts.
func (ac *AuthConnectors) restartDex(ctx context.Context) error {
	deploys := ac.Client.AppsV1().Deployments("dex")
	deploy, err := deploys.Get(ctx, "dex", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations["kipper.run/restartedAt"] = time.Now().Format(time.RFC3339)
	_, err = deploys.Update(ctx, deploy, metav1.UpdateOptions{})
	return err
}

func (ac *AuthConnectors) buildConnector(req connectorConfig, dexConfig map[string]interface{}) map[string]interface{} {
	issuer, _ := dexConfig["issuer"].(string)
	redirectURI := issuer + "/callback"

	switch req.Type {
	case "github":
		conn := map[string]interface{}{
			"type": "github",
			"id":   "github",
			"name": "GitHub",
			"config": map[string]interface{}{
				"clientID":     req.ClientID,
				"clientSecret": req.ClientSecret,
				"redirectURI":  redirectURI,
			},
		}
		if req.Org != "" {
			conn["config"].(map[string]interface{})["orgs"] = []map[string]string{{"name": req.Org}}
		}
		return conn

	case "gitlab":
		baseURL := "https://gitlab.com"
		if req.BaseURL != "" {
			baseURL = req.BaseURL
		}
		return map[string]interface{}{
			"type": "gitlab",
			"id":   "gitlab",
			"name": "GitLab",
			"config": map[string]interface{}{
				"clientID":     req.ClientID,
				"clientSecret": req.ClientSecret,
				"redirectURI":  redirectURI,
				"baseURL":      baseURL,
			},
		}

	case "google":
		conn := map[string]interface{}{
			"type": "google",
			"id":   "google",
			"name": "Google",
			"config": map[string]interface{}{
				"clientID":     req.ClientID,
				"clientSecret": req.ClientSecret,
				"redirectURI":  redirectURI,
			},
		}
		if req.Domain != "" {
			conn["config"].(map[string]interface{})["hostedDomains"] = []string{req.Domain}
		}
		return conn
	}

	return nil
}

func (ac *AuthConnectors) loadDexConfig(ctx context.Context) (map[string]interface{}, error) {
	cm, err := ac.Client.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseDexConfig(cm)
}

func parseDexConfig(cm *corev1.ConfigMap) (map[string]interface{}, error) {
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		return nil, err
	}
	return config, nil
}

// updateDexConfig applies change to the current Dex config and saves the result.
// Conflicts trigger a fresh read and another call to change, which must be safe
// to repeat. A false result or an error from change skips the ConfigMap write.
func (ac *AuthConnectors) updateDexConfig(ctx context.Context, change func(config map[string]interface{}) (bool, error)) error {
	configMaps := ac.Client.CoreV1().ConfigMaps("dex")
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := configMaps.Get(ctx, "dex-config", metav1.GetOptions{})
		if err != nil {
			return err
		}
		config, err := parseDexConfig(cm)
		if err != nil {
			return err
		}
		changed, err := change(config)
		if err != nil || !changed {
			return err
		}
		newYAML, err := yaml.Marshal(config)
		if err != nil {
			return err
		}
		cm.Data["config.yaml"] = string(newYAML)
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

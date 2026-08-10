package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// AuthConnectors handles OAuth connector configuration.
type AuthConnectors struct {
	Client kubernetes.Interface
}

type connectorConfig struct {
	Type         string `json:"type"` // github, gitlab, google
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Org          string `json:"org,omitempty"`      // GitHub org filter
	Domain       string `json:"domain,omitempty"`   // Google domain filter
	BaseURL      string `json:"base_url,omitempty"` // GitLab self-hosted URL
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

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	config, err := ac.loadDexConfig(ctx)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read auth config")
		return
	}

	connectors, _ := config["connectors"].([]interface{})

	// Remove existing connector of this type
	var filtered []interface{}
	for _, c := range connectors {
		if conn, ok := c.(map[string]interface{}); ok {
			if t, _ := conn["type"].(string); t == req.Type {
				continue
			}
		}
		filtered = append(filtered, c)
	}

	if req.Enabled && req.ClientID != "" {
		connector := ac.buildConnector(req, config)
		filtered = append(filtered, connector)
	}

	config["connectors"] = filtered

	if err := ac.saveDexConfig(ctx, config); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to save: %v", err))
		return
	}

	// Restart Dex
	deploy, err := ac.Client.AppsV1().Deployments("dex").Get(ctx, "dex", metav1.GetOptions{})
	if err == nil {
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = make(map[string]string)
		}
		deploy.Spec.Template.Annotations["kipper.run/restartedAt"] = time.Now().Format(time.RFC3339)
		_, _ = ac.Client.AppsV1().Deployments("dex").Update(ctx, deploy, metav1.UpdateOptions{})
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved"})
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
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		return nil, err
	}
	return config, nil
}

func (ac *AuthConnectors) saveDexConfig(ctx context.Context, config map[string]interface{}) error {
	cm, err := ac.Client.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return err
	}
	newYAML, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	cm.Data["config.yaml"] = string(newYAML)
	_, err = ac.Client.CoreV1().ConfigMaps("dex").Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

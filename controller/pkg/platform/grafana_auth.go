package platform

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// GrafanaNetworkPolicyName identifies the policy allowing ingress-controller
// and Prometheus pods to reach Grafana.
const GrafanaNetworkPolicyName = "grafana-ingress-only"

const (
	grafanaContainerPort   = 3000
	namespaceNameLabel     = "kubernetes.io/metadata.name"
	defaultIngressLabelKey = "app.kubernetes.io/name"
	defaultIngressLabelVal = "traefik"
)

// TraefikNamespace is where Kipper installs Traefik's pods: the chart's
// targetNamespace, not kube-system where its HelmChart object lives.
const TraefikNamespace = "traefik"

// KubePrometheusStackRelease is the Helm release name of the monitoring stack,
// which the chart puts on Grafana's pods as app.kubernetes.io/instance.
const KubePrometheusStackRelease = "kube-prometheus-stack"

// GrafanaPodLabels returns the labels used to select the chart's Grafana pods.
func GrafanaPodLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "grafana",
		"app.kubernetes.io/instance": KubePrometheusStackRelease,
	}
}

// IngressPeer identifies the ingress controller pods. Empty fields take the
// defaults for the Traefik Kipper installs; an empty namespace never means
// "any namespace".
type IngressPeer struct {
	Namespace  string
	LabelKey   string
	LabelValue string
}

// GrafanaNetworkPolicyObject admits traffic to Grafana's container port only
// from the ingress controller and from Prometheus, each pinned to its
// namespace and pod labels. In auth-proxy mode any other client that reached
// the port could claim any identity.
func GrafanaNetworkPolicyObject(peer IngressPeer) *networkingv1.NetworkPolicy {
	if peer.Namespace == "" {
		peer.Namespace = TraefikNamespace
	}
	if peer.LabelKey == "" || peer.LabelValue == "" {
		peer.LabelKey, peer.LabelValue = defaultIngressLabelKey, defaultIngressLabelVal
	}
	tcp := networkingv1.NetworkPolicyPort{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: grafanaContainerPort}}
	proto := corev1.ProtocolTCP
	tcp.Protocol = &proto
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: GrafanaNetworkPolicyName, Namespace: MonitoringNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: GrafanaPodLabels()},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				Ports: []networkingv1.NetworkPolicyPort{tcp},
				From: []networkingv1.NetworkPolicyPeer{
					pinnedPeer(peer.Namespace, peer.LabelKey, peer.LabelValue),
					pinnedPeer(MonitoringNamespace, "app.kubernetes.io/name", "prometheus"),
				},
			}},
		},
	}
}

// GrafanaNetworkPolicy renders GrafanaNetworkPolicyObject as YAML for kubectl.
func GrafanaNetworkPolicy(peer IngressPeer) string {
	out, err := yaml.Marshal(GrafanaNetworkPolicyObject(peer))
	if err != nil {
		panic(err)
	}
	return string(out)
}

func pinnedPeer(namespace, labelKey, labelValue string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{labelKey: labelValue}},
	}
}

package installer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// longhornSnapshot is what kip reads to decide whether Longhorn has settled.
type longhornSnapshot struct {
	Workloads        map[string]lhWorkload
	Settings         map[string]string
	Volumes          []lhVolume
	Replicas         []lhReplica
	Engines          []lhEngine
	EngineImages     []lhEngineImage
	InstanceManagers []lhInstanceManager
	LonghornNodes    []string
	Nodes            []NodeFacts
}

type lhWorkload struct {
	Kind    string
	Ready   bool
	Manager string
}

type lhVolume struct {
	Name         string
	Claim        string
	Replicas     int
	Image        string
	DataEngine   string
	State        string
	Robustness   string
	CurrentImage string
}

func (v lhVolume) label() string {
	if v.Claim != "" {
		return v.Claim
	}
	return v.Name
}

type lhReplica struct {
	Name, Volume, Node, HealthyAt, FailedAt, State string
}

type lhEngine struct {
	Volume     string
	State      string
	Modes      map[string]string
	Rebuilding bool
}

type lhEngineImage struct {
	Name               string
	Image              string
	State              string
	CLIAPI, Controller int
	RefCount           int
}

type lhInstanceManager struct {
	Node, Image, DataEngine, State string
}

// NodeFacts are the Kubernetes Node fields kip's Longhorn checks read.
type NodeFacts struct {
	Name           string
	KubeletVersion string
	OSImage        string
	Kernel         string
	OutOfService   bool
}

// outOfServiceTaint allows Kubernetes to move workloads without waiting for
// the node to respond. Fenced nodes carry this taint.
const outOfServiceTaint = "node.kubernetes.io/out-of-service"

const (
	longhornWorkloadQuery = `kubectl -n longhorn-system get daemonsets,deployments --request-timeout=30s -o json`
	longhornSettingQuery  = `kubectl -n longhorn-system get settings.longhorn.io --request-timeout=30s -o json`
	longhornObjectQuery   = `kubectl -n longhorn-system get volumes.longhorn.io,replicas.longhorn.io,engines.longhorn.io,engineimages.longhorn.io,instancemanagers.longhorn.io,nodes.longhorn.io --request-timeout=60s -o json`
	kubeNodeQuery         = `kubectl get nodes --request-timeout=30s -o json`
)

// readLonghornSnapshot reads Longhorn's workloads, settings, volumes, replicas,
// engines, engine images, instance managers and nodes, and the Kubernetes
// nodes. Any failed or unreadable read is an error.
func readLonghornSnapshot(client CommandRunner) (longhornSnapshot, error) {
	var snap longhornSnapshot
	for _, read := range []struct {
		what, query string
		parse       func([]byte, *longhornSnapshot) error
	}{
		{"Longhorn's workloads", longhornWorkloadQuery, parseLonghornWorkloads},
		{"Longhorn's settings", longhornSettingQuery, parseLonghornSettings},
		{"Longhorn's volumes", longhornObjectQuery, parseLonghornObjects},
		{"the cluster's nodes", kubeNodeQuery, parseKubeNodes},
	} {
		out, err := client.Run(read.query)
		if err != nil {
			return snap, fmt.Errorf("reading %s: %w", read.what, err)
		}
		if err := read.parse(jsonPayload(out), &snap); err != nil {
			return snap, fmt.Errorf("reading %s: %w", read.what, err)
		}
	}
	return snap, nil
}

// jsonPayload drops anything the SSH runner merged in before the JSON.
func jsonPayload(out string) []byte {
	if i := strings.Index(out, "{"); i > 0 {
		out = out[i:]
	}
	return []byte(out)
}

type kubeMeta struct {
	Name       string            `json:"name"`
	Generation int64             `json:"generation"`
	Labels     map[string]string `json:"labels"`
}

func parseLonghornWorkloads(data []byte, snap *longhornSnapshot) error {
	var list struct {
		Items []struct {
			Kind     string   `json:"kind"`
			Metadata kubeMeta `json:"metadata"`
			Spec     struct {
				Replicas *int32 `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `json:"name"`
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration     int64 `json:"observedGeneration"`
				Replicas               int32 `json:"replicas"`
				UpdatedReplicas        int32 `json:"updatedReplicas"`
				AvailableReplicas      int32 `json:"availableReplicas"`
				DesiredNumberScheduled int32 `json:"desiredNumberScheduled"`
				UpdatedNumberScheduled int32 `json:"updatedNumberScheduled"`
				NumberAvailable        int32 `json:"numberAvailable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	snap.Workloads = map[string]lhWorkload{}
	for _, it := range list.Items {
		st := it.Status
		w := lhWorkload{Kind: it.Kind}
		current := st.ObservedGeneration >= it.Metadata.Generation
		switch it.Kind {
		case "DaemonSet":
			w.Ready = current && st.DesiredNumberScheduled > 0 && st.UpdatedNumberScheduled == st.DesiredNumberScheduled && st.NumberAvailable == st.DesiredNumberScheduled
		case "Deployment":
			want := int32(1)
			if it.Spec.Replicas != nil {
				want = *it.Spec.Replicas
			}
			w.Ready = current && st.UpdatedReplicas == want && st.Replicas == want && st.AvailableReplicas >= want
		default:
			continue
		}
		for _, c := range it.Spec.Template.Spec.Containers {
			if c.Name == "longhorn-manager" {
				w.Manager = c.Image
			}
		}
		snap.Workloads[it.Kind+"/"+it.Metadata.Name] = w
	}
	return nil
}

func parseLonghornSettings(data []byte, snap *longhornSnapshot) error {
	var list struct {
		Items []struct {
			Metadata kubeMeta `json:"metadata"`
			Value    string   `json:"value"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	snap.Settings = map[string]string{}
	for _, it := range list.Items {
		snap.Settings[it.Metadata.Name] = it.Value
	}
	return nil
}

func parseLonghornObjects(data []byte, snap *longhornSnapshot) error {
	var list struct {
		Items []struct {
			Kind     string          `json:"kind"`
			Metadata kubeMeta        `json:"metadata"`
			Spec     json.RawMessage `json:"spec"`
			Status   json.RawMessage `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, it := range list.Items {
		var err error
		switch it.Kind {
		case "Volume":
			var spec struct {
				NumberOfReplicas int    `json:"numberOfReplicas"`
				Image            string `json:"image"`
				DataEngine       string `json:"dataEngine"`
			}
			var status struct {
				State            string `json:"state"`
				Robustness       string `json:"robustness"`
				CurrentImage     string `json:"currentImage"`
				KubernetesStatus struct {
					Namespace string `json:"namespace"`
					PVCName   string `json:"pvcName"`
				} `json:"kubernetesStatus"`
			}
			if err = unmarshalParts(it.Spec, &spec, it.Status, &status); err != nil {
				break
			}
			v := lhVolume{Name: it.Metadata.Name, Replicas: spec.NumberOfReplicas, Image: spec.Image, DataEngine: spec.DataEngine,
				State: status.State, Robustness: status.Robustness, CurrentImage: status.CurrentImage}
			if ks := status.KubernetesStatus; ks.Namespace != "" && ks.PVCName != "" {
				v.Claim = ks.Namespace + "/" + ks.PVCName
			}
			snap.Volumes = append(snap.Volumes, v)
		case "Replica":
			var spec struct {
				VolumeName string `json:"volumeName"`
				NodeID     string `json:"nodeID"`
				HealthyAt  string `json:"healthyAt"`
				FailedAt   string `json:"failedAt"`
			}
			var status struct {
				CurrentState string `json:"currentState"`
			}
			if err = unmarshalParts(it.Spec, &spec, it.Status, &status); err != nil {
				break
			}
			snap.Replicas = append(snap.Replicas, lhReplica{Name: it.Metadata.Name, Volume: spec.VolumeName, Node: spec.NodeID,
				HealthyAt: spec.HealthyAt, FailedAt: spec.FailedAt, State: status.CurrentState})
		case "Engine":
			var spec struct {
				VolumeName string `json:"volumeName"`
			}
			var status struct {
				CurrentState   string            `json:"currentState"`
				ReplicaModeMap map[string]string `json:"replicaModeMap"`
				RebuildStatus  map[string]*struct {
					IsRebuilding bool `json:"isRebuilding"`
				} `json:"rebuildStatus"`
			}
			if err = unmarshalParts(it.Spec, &spec, it.Status, &status); err != nil {
				break
			}
			e := lhEngine{Volume: spec.VolumeName, State: status.CurrentState, Modes: status.ReplicaModeMap}
			for _, r := range status.RebuildStatus {
				if r != nil && r.IsRebuilding {
					e.Rebuilding = true
				}
			}
			snap.Engines = append(snap.Engines, e)
		case "EngineImage":
			var spec struct {
				Image string `json:"image"`
			}
			var status struct {
				State                string `json:"state"`
				CLIAPIVersion        int    `json:"cliAPIVersion"`
				ControllerAPIVersion int    `json:"controllerAPIVersion"`
				RefCount             int    `json:"refCount"`
			}
			if err = unmarshalParts(it.Spec, &spec, it.Status, &status); err != nil {
				break
			}
			snap.EngineImages = append(snap.EngineImages, lhEngineImage{Name: it.Metadata.Name, Image: spec.Image, State: status.State,
				CLIAPI: status.CLIAPIVersion, Controller: status.ControllerAPIVersion, RefCount: status.RefCount})
		case "InstanceManager":
			var spec struct {
				Image      string `json:"image"`
				NodeID     string `json:"nodeID"`
				DataEngine string `json:"dataEngine"`
			}
			var status struct {
				CurrentState string `json:"currentState"`
			}
			if err = unmarshalParts(it.Spec, &spec, it.Status, &status); err != nil {
				break
			}
			snap.InstanceManagers = append(snap.InstanceManagers, lhInstanceManager{Node: spec.NodeID, Image: spec.Image,
				DataEngine: spec.DataEngine, State: status.CurrentState})
		case "Node":
			snap.LonghornNodes = append(snap.LonghornNodes, it.Metadata.Name)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", it.Kind, it.Metadata.Name, err)
		}
	}
	return nil
}

func unmarshalParts(spec json.RawMessage, specInto any, status json.RawMessage, statusInto any) error {
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, specInto); err != nil {
			return err
		}
	}
	if len(status) > 0 {
		if err := json.Unmarshal(status, statusInto); err != nil {
			return err
		}
	}
	return nil
}

func parseKubeNodes(data []byte, snap *longhornSnapshot) error {
	nodes, err := ParseNodeFacts(data)
	snap.Nodes = nodes
	return err
}

// ParseNodeFacts reads a Kubernetes NodeList in JSON, ignoring text before
// the first opening brace. It preserves node order and returns nil for an
// empty list. Missing fields retain their zero values. Malformed JSON or
// incompatible field types return an error.
func ParseNodeFacts(data []byte) ([]NodeFacts, error) {
	var list struct {
		Items []struct {
			Metadata kubeMeta `json:"metadata"`
			Spec     struct {
				Taints []struct {
					Key string `json:"key"`
				} `json:"taints"`
			} `json:"spec"`
			Status struct {
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
					OSImage        string `json:"osImage"`
					KernelVersion  string `json:"kernelVersion"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(jsonPayload(string(data)), &list); err != nil {
		return nil, err
	}
	var nodes []NodeFacts
	for _, it := range list.Items {
		ni := it.Status.NodeInfo
		f := NodeFacts{Name: it.Metadata.Name, KubeletVersion: ni.KubeletVersion, OSImage: ni.OSImage, Kernel: ni.KernelVersion}
		for _, taint := range it.Spec.Taints {
			f.OutOfService = f.OutOfService || taint.Key == outOfServiceTaint
		}
		nodes = append(nodes, f)
	}
	return nodes, nil
}

// longhornSettledWorkloads are the Longhorn workloads a settled release runs.
var longhornSettledWorkloads = []string{
	"DaemonSet/longhorn-manager",
	"DaemonSet/longhorn-csi-plugin",
	"Deployment/longhorn-driver-deployer",
	"Deployment/csi-attacher",
	"Deployment/csi-provisioner",
	"Deployment/csi-resizer",
	"Deployment/csi-snapshotter",
}

// componentProblems lists what keeps Longhorn's own components from having
// settled at version: the manager, CSI, the default engine image and an
// instance manager on every Longhorn node.
func (s longhornSnapshot) componentProblems(version string) []string {
	var problems []string
	for _, name := range longhornSettledWorkloads {
		w, ok := s.Workloads[name]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s does not exist yet", name))
		case !w.Ready:
			problems = append(problems, fmt.Sprintf("%s has not finished rolling out", name))
		}
	}
	if m := s.Workloads["DaemonSet/longhorn-manager"].Manager; m != "" && !strings.HasSuffix(m, "longhorn-manager:"+version) {
		problems = append(problems, fmt.Sprintf("longhorn-manager runs %s, not %s", m, version))
	}
	if running := s.Settings["current-longhorn-version"]; running != version {
		problems = append(problems, fmt.Sprintf("longhorn-manager has not finished upgrading to %s (current-longhorn-version is %q)", version, running))
	}
	engine := s.Settings["default-engine-image"]
	switch {
	case !strings.HasSuffix(engine, "longhorn-engine:"+version):
		problems = append(problems, fmt.Sprintf("the default engine image is %q, not %s's", engine, version))
	case s.engineImage(engine) == nil:
		problems = append(problems, fmt.Sprintf("the engine image %s has no EngineImage yet", engine))
	case s.engineImage(engine).State != "deployed":
		problems = append(problems, fmt.Sprintf("the engine image %s is %q, not deployed", engine, s.engineImage(engine).State))
	}
	im := s.Settings["default-instance-manager-image"]
	if im == "" {
		problems = append(problems, "the default instance manager image is not set")
	}
	if len(s.LonghornNodes) == 0 {
		problems = append(problems, "Longhorn lists no nodes")
	}
	for _, node := range s.LonghornNodes {
		running := false
		for _, m := range s.InstanceManagers {
			if m.Node == node && m.Image == im && m.DataEngine != "v2" && m.State == "running" {
				running = true
			}
		}
		switch {
		case running:
		case s.outOfService(node):
			problems = append(problems, outOfServiceProblem(node))
		default:
			problems = append(problems, fmt.Sprintf("node %s has no running instance manager on %s", node, im))
		}
	}
	return problems
}

// outOfServiceProblems identifies Longhorn nodes with the out-of-service taint
// used by fencing, so a resumed step reports them before reapplying the manifest.
func (s longhornSnapshot) outOfServiceProblems() []string {
	var problems []string
	for _, node := range s.LonghornNodes {
		if s.outOfService(node) {
			problems = append(problems, outOfServiceProblem(node))
		}
	}
	return problems
}

func outOfServiceProblem(node string) string {
	return fmt.Sprintf("node %s is out of service, which blocks the Longhorn upgrade. Bring it back with kip node add, or remove it permanently with kip node remove --force", node)
}

func (s longhornSnapshot) outOfService(node string) bool {
	for _, n := range s.Nodes {
		if n.Name == node {
			return n.OutOfService
		}
	}
	return false
}

func (s longhornSnapshot) engineImage(image string) *lhEngineImage {
	for i := range s.EngineImages {
		if s.EngineImages[i].Image == image {
			return &s.EngineImages[i]
		}
	}
	return nil
}

func (s longhornSnapshot) engineFor(volume string) (lhEngine, bool) {
	for _, e := range s.Engines {
		if e.Volume == volume {
			return e, true
		}
	}
	return lhEngine{}, false
}

// degradedByDesign reports whether an attached volume is degraded only because
// it wants more replicas than the cluster has Longhorn nodes, while every node
// holds a running, healthy replica that its engine uses read-write.
func (s longhornSnapshot) degradedByDesign(v lhVolume) bool {
	if v.State != "attached" || v.Robustness != "degraded" || v.Replicas <= len(s.LonghornNodes) {
		return false
	}
	engine, ok := s.engineFor(v.Name)
	if !ok {
		return false
	}
	for _, node := range s.LonghornNodes {
		usable := false
		for _, r := range s.Replicas {
			if r.Volume == v.Name && r.Node == node && r.State == "running" && r.HealthyAt != "" && r.FailedAt == "" && engine.Modes[r.Name] == "RW" {
				usable = true
			}
		}
		if !usable {
			return false
		}
	}
	return true
}

// volumeProblems lists volumes that are not healthy and stable: faulted,
// v2, in transition, attached with unknown health, rebuilding, degraded other
// than by design, or with an engine upgrade in progress. A detached volume's
// unknown robustness is normal.
func (s longhornSnapshot) volumeProblems() []string {
	problems := s.carryProblems()
	for _, v := range s.Volumes {
		switch {
		case v.DataEngine == "v2" || v.Robustness == "faulted":
			continue
		case v.State != "attached" && v.State != "detached":
			problems = append(problems, fmt.Sprintf("%s is %s", v.label(), orUnknown(v.State)))
		case v.State == "attached" && v.Robustness == "unknown":
			problems = append(problems, fmt.Sprintf("%s is attached with unknown health", v.label()))
		case v.State == "attached" && v.Robustness != "healthy" && !s.degradedByDesign(v):
			problems = append(problems, fmt.Sprintf("%s is %s", v.label(), orUnknown(v.Robustness)))
		case v.Image != v.CurrentImage:
			problems = append(problems, fmt.Sprintf("%s is moving from engine %s to %s", v.label(), v.CurrentImage, v.Image))
		case v.State == "attached":
			if p := s.engineEvidenceProblem(v); p != "" {
				problems = append(problems, p)
			}
		}
	}
	for _, e := range s.Engines {
		if e.Rebuilding {
			problems = append(problems, fmt.Sprintf("a replica of %s is rebuilding", s.volumeLabel(e.Volume)))
			continue
		}
		for _, mode := range e.Modes {
			if mode != "RW" {
				problems = append(problems, fmt.Sprintf("a replica of %s is %s, not RW", s.volumeLabel(e.Volume), mode))
				break
			}
		}
	}
	return problems
}

// carryProblems lists the volumes no Longhorn step may carry forward: those
// on the v2 data engine and those that are faulted.
func (s longhornSnapshot) carryProblems() []string {
	var problems []string
	for _, v := range s.Volumes {
		switch {
		case v.DataEngine == "v2":
			problems = append(problems, fmt.Sprintf("%s uses Longhorn's v2 data engine, which kip does not upgrade", v.label()))
		case v.Robustness == "faulted":
			problems = append(problems, fmt.Sprintf("%s is faulted", v.label()))
		}
	}
	return problems
}

// engineEvidenceProblem checks an attached volume against its engine and
// replica records. It requires one running engine and at least one healthy,
// running, read-write replica. A volume marked healthy must have at least
// its requested replica count. An empty string means these checks pass.
func (s longhornSnapshot) engineEvidenceProblem(v lhVolume) string {
	var engines []lhEngine
	for _, e := range s.Engines {
		if e.Volume == v.Name {
			engines = append(engines, e)
		}
	}
	switch {
	case len(engines) == 0:
		return fmt.Sprintf("%s is attached, but Longhorn lists no engine for it", v.label())
	case len(engines) > 1:
		return fmt.Sprintf("%s has %d engines, so Longhorn is still moving it", v.label(), len(engines))
	case engines[0].State != "running":
		return fmt.Sprintf("%s is attached, but its engine is %s", v.label(), orUnknown(engines[0].State))
	}
	usable := 0
	for _, r := range s.Replicas {
		if r.Volume == v.Name && engines[0].Modes[r.Name] == "RW" && r.State == "running" && r.HealthyAt != "" && r.FailedAt == "" {
			usable++
		}
	}
	switch {
	case usable == 0:
		return fmt.Sprintf("%s is attached, but none of its replicas is running and read-write", v.label())
	case v.Robustness == "healthy" && usable < v.Replicas:
		return fmt.Sprintf("%s is healthy, but only %d of its %d replicas are running and read-write", v.label(), usable, v.Replicas)
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "in an unknown state"
	}
	return s
}

func (s longhornSnapshot) volumeLabel(name string) string {
	for _, v := range s.Volumes {
		if v.Name == name {
			return v.label()
		}
	}
	return name
}

// engineLag separates volumes using a non-default engine into those eligible for
// upgrade and attached volumes accepted by degradedByDesign. The latter keep
// their engine until they detach or have enough nodes for all their replicas.
func (s longhornSnapshot) engineLag() (lagging, exempt []string) {
	engine := s.Settings["default-engine-image"]
	for _, v := range s.Volumes {
		if v.Image == engine && v.CurrentImage == engine {
			continue
		}
		if s.degradedByDesign(v) {
			exempt = append(exempt, v.label())
		} else {
			lagging = append(lagging, v.label())
		}
	}
	return lagging, exempt
}

// readyProblems lists component and volume problems at version, allowing
// stable older engines and volumes accepted by degradedByDesign.
func (s longhornSnapshot) readyProblems(version string) []string {
	return append(s.componentProblems(version), s.volumeProblems()...)
}

// settledProblems adds volumes still on older engines to readyProblems,
// except those accepted by degradedByDesign. These added problems are sorted
// by volume label.
func (s longhornSnapshot) settledProblems(version string) []string {
	problems := s.readyProblems(version)
	lagging, _ := s.engineLag()
	sort.Strings(lagging)
	for _, v := range lagging {
		problems = append(problems, fmt.Sprintf("%s does not run the default engine yet", v))
	}
	return problems
}

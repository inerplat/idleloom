package v1alpha1

import (
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	WorkloadModeServer = "Server"
	WorkloadModeBatch  = "Batch"
	WorkloadModeShell  = "Shell"
	WorkloadModeTrain  = "Train"

	RuntimeProfileMLXLMV1               = "mlx-lm-v1"
	RuntimeProfileOllamaGGUFV1          = "ollama-gguf-v1"
	RuntimeProfileLlamaCppMetalV1       = "llama-cpp-metal-v1"
	RuntimeProfileMLXTrainV1            = "mlx-train-v1"
	RuntimeProfileShellV1               = "shell-v1"
	ModelFamilyQwen35                   = "qwen3.5"
	ModelFamilyOllamaGGUF               = "ollama-gguf"
	ModelFamilyGGUF                     = "gguf"
	ArtifactFormatSafetensorsV1         = "mlx-safetensors-v1"
	ArtifactFormatGGUFV1                = "gguf-v1"
	AgentProtocolV1Alpha1               = "native.ai.idleloom.io/v1alpha1"
	CapabilityBatchInferenceV1          = "BatchInferenceV1"
	CapabilityNativeServiceV1           = "NativeServiceV1"
	CapabilityNativeTrainingV1          = "NativeTrainingV1"
	ServingAuthSecretName               = "active-serve-auth"
	NativeServingPort             int32 = 18080

	KrunkitStateStopped = "Stopped"
	KrunkitStateRunning = "Running"
	KrunkitStateUnknown = "Unknown"

	AssignmentDesiredRunning = "Running"
	AssignmentDesiredStopped = "Stopped"
	AssignmentMailboxName    = "active"
	WorkloadFinalizer        = "native.ai.idleloom.io/stop"

	HostConditionReady           = "Ready"
	HostConditionDevelopmentOnly = "DevelopmentOnly"
	HostConditionConnected       = "Connected"
	WorkloadConditionReady       = "Ready"

	ConnectivityModeAPIOnly      = "APIOnly"
	ConnectivityModeWireKubeLeaf = "WireKubeLeaf"
	ConnectivityProviderWireKube = "WireKube"
	ConnectivityTransportRelay   = "Relay"
	ShellAccessDisabled          = "Disabled"
	ShellAccessSandboxed         = "Sandboxed"
	ShellAccessHost              = "Host"
	ShellIsolationSandbox        = "Sandbox"
	ShellIsolationHost           = "Host"
	ShellNetworkNone             = "None"
	ShellNetworkOutbound         = "Outbound"

	PhaseScheduling = "Scheduling"
	PhaseAssigned   = "Assigned"
	PhaseStarting   = "Starting"
	PhaseRunning    = "Running"
	PhaseBlocked    = "Blocked"
	PhaseFailed     = "Failed"
	PhaseFenced     = "Fenced"
	PhaseStopped    = "Stopped"
	PhaseSucceeded  = "Succeeded"
)

const (
	DefaultAgentHeartbeatTimeout = 45 * time.Second
	HeartbeatClockSkewAllowance  = time.Minute
)

type IdleloomWorkloadSpec struct {
	// +kubebuilder:validation:Enum=Server;Batch;Shell;Train
	Mode      string                  `json:"mode"`
	Model     *WorkloadModelReference `json:"model,omitempty"`
	Server    *WorkloadServer         `json:"server,omitempty"`
	Batch     *WorkloadBatchInference `json:"batch,omitempty"`
	Shell     *WorkloadShell          `json:"shell,omitempty"`
	Train     *WorkloadTraining       `json:"train,omitempty"`
	Run       *WorkloadRunSpec        `json:"run,omitempty"`
	Resources WorkloadResources       `json:"resources"`
}

// +kubebuilder:validation:MaxLength=4096
// +kubebuilder:validation:Pattern=`^[^\x00]*$`
type WorkloadRunParameter string

type WorkloadRunSpec struct {
	// +kubebuilder:validation:Enum=train;infer;serve;shell
	Task string `json:"task"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Experiment string `json:"experiment"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Attempt int32 `json:"attempt"`
	// Parameters are non-secret environment values recorded with the immutable run.
	// +kubebuilder:validation:MaxProperties=64
	Parameters map[string]WorkloadRunParameter `json:"parameters,omitempty"`
}

type WorkloadTraining struct {
	// +kubebuilder:validation:Enum=mlx-train-v1
	RuntimeProfile string                 `json:"runtimeProfile"`
	Source         WorkloadTrainingSource `json:"source"`
	// +kubebuilder:validation:Enum=None;Outbound
	Network string `json:"network,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type WorkloadTrainingSource struct {
	// Inline contains one immutable Python program executed with the locked MLX runtime.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	Inline string `json:"inline"`
}

type WorkloadModelReference struct {
	// CatalogRef names a cluster-scoped IdleloomModel curated by an operator.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	CatalogRef string `json:"catalogRef"`
}

type WorkloadServer struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	ServiceName string `json:"serviceName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ModelAlias string `json:"modelAlias"`
}

type WorkloadBatchInference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16384
	Prompt string `json:"prompt"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	MaxTokens int32 `json:"maxTokens"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type WorkloadShell struct {
	// Script is executed by /bin/zsh -lc inside the restricted Native sandbox.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	Script string `json:"script"`
	// +kubebuilder:validation:Enum=Sandbox;Host
	Isolation string `json:"isolation,omitempty"`
	// +kubebuilder:validation:Enum=None;Outbound
	Network string `json:"network,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type WorkloadResources struct {
	// UnifiedMemoryRequest is a reservation hint, not an enforced memory limit.
	UnifiedMemoryRequest resource.Quantity `json:"unifiedMemoryRequest"`
}

type IdleloomWorkloadStatus struct {
	ObservedGeneration     int64                      `json:"observedGeneration,omitempty"`
	Phase                  string                     `json:"phase,omitempty"`
	SchedulingIntent       *WorkloadSchedulingIntent  `json:"schedulingIntent,omitempty"`
	AssignmentRef          *NamespacedObjectReference `json:"assignmentRef,omitempty"`
	ResolvedArtifactDigest string                     `json:"resolvedArtifactDigest,omitempty"`
	Run                    *WorkloadRunStatus         `json:"run,omitempty"`
	Conditions             []metav1.Condition         `json:"conditions,omitempty"`
}

type WorkloadRunStatus struct {
	// +kubebuilder:validation:Format=uuid
	ID string `json:"id,omitempty"`
	// +kubebuilder:validation:Enum=train;infer;serve;shell
	Task string `json:"task,omitempty"`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Experiment string `json:"experiment,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Attempt    int32             `json:"attempt,omitempty"`
	StartedAt  *metav1.MicroTime `json:"startedAt,omitempty"`
	FinishedAt *metav1.MicroTime `json:"finishedAt,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	Metrics []RunMetricSummary `json:"metrics,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Artifacts []RunArtifactReference `json:"artifacts,omitempty"`
}

type RunMetricSummary struct {
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`
	Value string `json:"value"`
	// +kubebuilder:validation:Minimum=0
	Step       int64            `json:"step,omitempty"`
	ObservedAt metav1.MicroTime `json:"observedAt"`
}

type RunArtifactReference struct {
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:Pattern=`^[^\x00]*$`
	URI string `json:"uri"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=68719476736
	SizeBytes int64 `json:"sizeBytes,omitempty"`
}

// WorkloadSchedulingIntent is durably written with resourceVersion CAS before
// the controller creates an Assignment. It lets reconciliation resume without
// allocating a second fencing epoch or changing the selected execution.
type WorkloadSchedulingIntent struct {
	WorkloadGeneration int64                     `json:"workloadGeneration"`
	HostRef            NamespacedObjectReference `json:"hostRef"`
	ModelRef           *ObjectReference          `json:"modelRef,omitempty"`
	ExecutionID        string                    `json:"executionID"`
	FencingEpoch       int64                     `json:"fencingEpoch"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=idleloomworkloads,scope=Namespaced,shortName=ilw
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.catalogRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="spec is immutable; create a new workload to change it"
// +kubebuilder:validation:XValidation:rule="quantity(self.spec.resources.unifiedMemoryRequest).isGreaterThan(quantity('0'))",message="unifiedMemoryRequest must be positive"
// +kubebuilder:validation:XValidation:rule="(self.spec.mode == 'Server' && has(self.spec.model) && has(self.spec.server) && !has(self.spec.batch) && !has(self.spec.shell) && !has(self.spec.train)) || (self.spec.mode == 'Batch' && has(self.spec.model) && !has(self.spec.server) && has(self.spec.batch) && !has(self.spec.shell) && !has(self.spec.train)) || (self.spec.mode == 'Shell' && !has(self.spec.model) && !has(self.spec.server) && !has(self.spec.batch) && has(self.spec.shell) && !has(self.spec.train)) || (self.spec.mode == 'Train' && !has(self.spec.model) && !has(self.spec.server) && !has(self.spec.batch) && !has(self.spec.shell) && has(self.spec.train) && has(self.spec.run) && self.spec.run.task == 'train')",message="mode-specific model, server, batch, shell, and train fields are inconsistent"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.shell) || self.spec.shell.isolation != 'Host' || !has(self.spec.shell.network) || self.spec.shell.network == 'Outbound'",message="Host shell isolation requires outbound network access"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.run) || (self.spec.mode == 'Server' && self.spec.run.task == 'serve') || (self.spec.mode == 'Batch' && self.spec.run.task == 'infer') || (self.spec.mode == 'Shell' && self.spec.run.task == 'shell') || (self.spec.mode == 'Train' && self.spec.run.task == 'train')",message="run task must match workload mode"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.run) || !has(self.spec.run.parameters) || self.spec.run.parameters.all(k, k.matches('^[A-Z][A-Z0-9_]{0,62}$') && !k.startsWith('IDLELOOM_'))",message="run parameter names must be safe environment names and must not use IDLELOOM_"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.train) || !self.spec.train.source.inline.contains('\\u0000')",message="training source must not contain NUL bytes"
type IdleloomWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IdleloomWorkloadSpec   `json:"spec"`
	Status            IdleloomWorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IdleloomWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdleloomWorkload `json:"items"`
}

type IdleloomModelSpec struct {
	// +kubebuilder:validation:Enum=qwen3.5;ollama-gguf;gguf
	Family string `json:"family"`
	// +kubebuilder:validation:Enum=mlx-lm-v1;ollama-gguf-v1;llama-cpp-metal-v1
	RuntimeProfile       string            `json:"runtimeProfile"`
	Artifact             ModelArtifact     `json:"artifact"`
	MinimumUnifiedMemory resource.Quantity `json:"minimumUnifiedMemory"`
	// +kubebuilder:validation:Minimum=128
	// +kubebuilder:validation:Maximum=8192
	MaxContextLength int32 `json:"maxContextLength"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
	MaxConcurrentRequests int32 `json:"maxConcurrentRequests"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.ociReference) && !has(self.ollamaModel) && !has(self.ggufFile) && has(self.signature)) || (!has(self.ociReference) && has(self.ollamaModel) && !has(self.ggufFile) && !has(self.signature)) || (!has(self.ociReference) && !has(self.ollamaModel) && has(self.ggufFile) && !has(self.signature))",message="exactly one artifact source is required; OCI artifacts require a signature and local GGUF sources are unsigned"
type ModelArtifact struct {
	// OCIReference must be pinned by digest. Tags are not accepted.
	// +kubebuilder:validation:Pattern=`^oci://[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`
	OCIReference string `json:"ociReference,omitempty"`
	// OllamaModel is an already-installed local model. Idleloom never pulls it.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9._-]{0,127}:[a-z0-9][a-z0-9._-]{0,63}$`
	OllamaModel string `json:"ollamaModel,omitempty"`
	// GGUFFile is a filename under the host's managed llama.cpp model directory.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,190}\.gguf$`
	GGUFFile string `json:"ggufFile,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ManifestDigest string `json:"manifestDigest"`
	// +kubebuilder:validation:Enum=mlx-safetensors-v1;gguf-v1
	Format string `json:"format"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=68719476736
	SizeBytes int64            `json:"sizeBytes"`
	Signature *SignaturePolicy `json:"signature,omitempty"`
}

type SignaturePolicy struct {
	// Issuer and Subject identify the trusted signer required for the OCI artifact.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`.*[[:graph:]].*`
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`.*[[:graph:]].*`
	Subject string `json:"subject"`
}

type IdleloomModelStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=idleloommodels,scope=Cluster,shortName=ilm
// +kubebuilder:printcolumn:name="Family",type=string,JSONPath=`.spec.family`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtimeProfile`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="catalog model specs are immutable"
// +kubebuilder:validation:XValidation:rule="quantity(self.spec.minimumUnifiedMemory).isGreaterThan(quantity('0'))",message="minimumUnifiedMemory must be positive"
// +kubebuilder:validation:XValidation:rule="(self.spec.runtimeProfile == 'mlx-lm-v1' && self.spec.family == 'qwen3.5' && self.spec.artifact.format == 'mlx-safetensors-v1' && has(self.spec.artifact.ociReference)) || (self.spec.runtimeProfile == 'ollama-gguf-v1' && self.spec.family == 'ollama-gguf' && self.spec.artifact.format == 'gguf-v1' && has(self.spec.artifact.ollamaModel)) || (self.spec.runtimeProfile == 'llama-cpp-metal-v1' && self.spec.family == 'gguf' && self.spec.artifact.format == 'gguf-v1' && has(self.spec.artifact.ggufFile))",message="runtime profile, model family, and artifact source must match"
type IdleloomModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IdleloomModelSpec   `json:"spec"`
	Status            IdleloomModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IdleloomModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdleloomModel `json:"items"`
}

type IdleloomHostSpec struct {
	// AgentID is the expected identity for the agent using this host mailbox.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	AgentID string `json:"agentID"`
	// ShellAccess defines the maximum remote shell privilege this host accepts.
	// +kubebuilder:validation:Enum=Disabled;Sandboxed;Host
	ShellAccess string `json:"shellAccess,omitempty"`
}

type IdleloomHostStatus struct {
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	ProtocolVersion    string   `json:"protocolVersion,omitempty"`
	RuntimeProfiles    []string `json:"runtimeProfiles,omitempty"`
	ModelFamilies      []string `json:"modelFamilies,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	AvailableModels          []HostModelStatus `json:"availableModels,omitempty"`
	Capabilities             []string          `json:"capabilities,omitempty"`
	AllocatableUnifiedMemory resource.Quantity `json:"allocatableUnifiedMemory,omitempty"`
	AvailableUnifiedMemory   resource.Quantity `json:"availableUnifiedMemory,omitempty"`
	// +kubebuilder:validation:Enum=Stopped;Running;Unknown
	KrunkitState        string                  `json:"krunkitState,omitempty"`
	VulkanLeaseActive   bool                    `json:"vulkanLeaseActive,omitempty"`
	ActiveAssignmentUID types.UID               `json:"activeAssignmentUID,omitempty"`
	LastHeartbeatTime   *metav1.MicroTime       `json:"lastHeartbeatTime,omitempty"`
	Connectivity        *HostConnectivityStatus `json:"connectivity,omitempty"`
	Conditions          []metav1.Condition      `json:"conditions,omitempty"`
}

type HostModelStatus struct {
	// +kubebuilder:validation:Enum=ollama-gguf-v1;llama-cpp-metal-v1
	RuntimeProfile string `json:"runtimeProfile"`
	// +kubebuilder:validation:Pattern=`^([a-z0-9][a-z0-9._-]{0,127}:[a-z0-9][a-z0-9._-]{0,63}|[A-Za-z0-9][A-Za-z0-9._-]{0,190}\.gguf)$`
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ManifestDigest string `json:"manifestDigest"`
	// +kubebuilder:validation:Enum=qwen3.5;ollama-gguf;gguf
	Family string `json:"family"`
	// +kubebuilder:validation:Enum=gguf-v1
	Format string `json:"format"`
	// +kubebuilder:validation:Minimum=1
	SizeBytes int64 `json:"sizeBytes"`
}

type HostConnectivityStatus struct {
	// +kubebuilder:validation:Enum=APIOnly;WireKubeLeaf
	Mode string `json:"mode,omitempty"`
	// +kubebuilder:validation:Enum=WireKube
	Provider string `json:"provider,omitempty"`
	// +kubebuilder:validation:Enum=Relay
	Transport         string            `json:"transport,omitempty"`
	PeerName          string            `json:"peerName,omitempty"`
	Address           string            `json:"address,omitempty"`
	InterfaceName     string            `json:"interfaceName,omitempty"`
	LastHandshakeTime *metav1.MicroTime `json:"lastHandshakeTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=idleloomhosts,scope=Namespaced,shortName=ilh
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentID`
// +kubebuilder:printcolumn:name="Krunkit",type=string,JSONPath=`.status.krunkitState`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.availableUnifiedMemory`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="host identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'host'",message="host mailbox object must be named host"
type IdleloomHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IdleloomHostSpec   `json:"spec"`
	Status            IdleloomHostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IdleloomHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdleloomHost `json:"items"`
}

type WorkloadObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

type ObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type NamespacedObjectReference struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.server) != has(self.batch)",message="exactly one server or batch intent is required"
// +kubebuilder:validation:XValidation:rule="(self.runtimeProfile == 'mlx-lm-v1' && self.family == 'qwen3.5' && self.artifact.format == 'mlx-safetensors-v1' && has(self.artifact.ociReference)) || (self.runtimeProfile == 'ollama-gguf-v1' && self.family == 'ollama-gguf' && self.artifact.format == 'gguf-v1' && has(self.artifact.ollamaModel)) || (self.runtimeProfile == 'llama-cpp-metal-v1' && self.family == 'gguf' && self.artifact.format == 'gguf-v1' && has(self.artifact.ggufFile))",message="resolved runtime profile, model family, and artifact source must match"
type ResolvedModel struct {
	CatalogRef ObjectReference `json:"catalogRef"`
	// +kubebuilder:validation:Enum=qwen3.5;ollama-gguf;gguf
	Family string `json:"family"`
	// +kubebuilder:validation:Enum=mlx-lm-v1;ollama-gguf-v1;llama-cpp-metal-v1
	RuntimeProfile       string            `json:"runtimeProfile"`
	Artifact             ModelArtifact     `json:"artifact"`
	UnifiedMemoryRequest resource.Quantity `json:"unifiedMemoryRequest"`
	// +kubebuilder:validation:Minimum=128
	// +kubebuilder:validation:Maximum=8192
	MaxContextLength int32 `json:"maxContextLength"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
	MaxConcurrentRequests int32                   `json:"maxConcurrentRequests"`
	Server                *ResolvedServer         `json:"server,omitempty"`
	Batch                 *WorkloadBatchInference `json:"batch,omitempty"`
}

type ResolvedServer struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	ServiceName string `json:"serviceName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ModelAlias string `json:"modelAlias"`
	// +kubebuilder:validation:Enum=active-serve-auth
	AuthSecretName string `json:"authSecretName"`
	// +kubebuilder:validation:Minimum=18080
	// +kubebuilder:validation:Maximum=18080
	Port int32 `json:"port"`
}

type ResolvedShell struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	// +kubebuilder:validation:Pattern=`^[^\x00]*$`
	Script string `json:"script"`
	// +kubebuilder:validation:Enum=Sandbox;Host
	Isolation string `json:"isolation"`
	// +kubebuilder:validation:Enum=None;Outbound
	Network string `json:"network"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds       int32             `json:"timeoutSeconds"`
	UnifiedMemoryRequest resource.Quantity `json:"unifiedMemoryRequest"`
}

type ResolvedTraining struct {
	// +kubebuilder:validation:Enum=mlx-train-v1
	RuntimeProfile string `json:"runtimeProfile"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=65536
	// +kubebuilder:validation:Pattern=`^[^\x00]*$`
	Source string `json:"source"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	SourceDigest string `json:"sourceDigest"`
	// +kubebuilder:validation:Enum=None;Outbound
	Network string `json:"network"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds       int32             `json:"timeoutSeconds"`
	UnifiedMemoryRequest resource.Quantity `json:"unifiedMemoryRequest"`
}

type IdleloomWorkloadAssignmentSpec struct {
	// +kubebuilder:validation:Enum=Running;Stopped
	DesiredState string                  `json:"desiredState"`
	WorkloadRef  WorkloadObjectReference `json:"workloadRef"`
	HostRef      ObjectReference         `json:"hostRef"`
	Model        *ResolvedModel          `json:"model,omitempty"`
	Shell        *ResolvedShell          `json:"shell,omitempty"`
	Training     *ResolvedTraining       `json:"training,omitempty"`
	Run          *WorkloadRunSpec        `json:"run,omitempty"`
	// +kubebuilder:validation:Format=uuid
	ExecutionID string `json:"executionID"`
	// +kubebuilder:validation:Minimum=1
	FencingEpoch int64 `json:"fencingEpoch"`
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=300
	LeaseDurationSeconds int32 `json:"leaseDurationSeconds"`
}

type IdleloomWorkloadAssignmentStatus struct {
	ObservedGeneration     int64                `json:"observedGeneration,omitempty"`
	Phase                  string               `json:"phase,omitempty"`
	AgentID                string               `json:"agentID,omitempty"`
	ExecutionID            string               `json:"executionID,omitempty"`
	FencingEpoch           int64                `json:"fencingEpoch,omitempty"`
	RuntimeVersion         string               `json:"runtimeVersion,omitempty"`
	ResolvedArtifactDigest string               `json:"resolvedArtifactDigest,omitempty"`
	Run                    *WorkloadRunStatus   `json:"run,omitempty"`
	LastHeartbeatTime      *metav1.MicroTime    `json:"lastHeartbeatTime,omitempty"`
	StopAcknowledgement    *StopAcknowledgement `json:"stopAcknowledgement,omitempty"`
	Conditions             []metav1.Condition   `json:"conditions,omitempty"`
}

// StopAcknowledgement proves that the exact fenced execution represented by
// this Assignment has stopped. Controllers must not accept a generic Stopped
// phase when releasing the workload finalizer.
type StopAcknowledgement struct {
	AssignmentUID      types.UID        `json:"assignmentUID"`
	ObservedGeneration int64            `json:"observedGeneration"`
	ExecutionID        string           `json:"executionID"`
	FencingEpoch       int64            `json:"fencingEpoch"`
	StoppedAt          metav1.MicroTime `json:"stoppedAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=idleloomworkloadassignments,scope=Namespaced,shortName=ilwa
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workloadRef.name`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredState`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:validation:XValidation:rule="self.spec.workloadRef == oldSelf.spec.workloadRef",message="workload identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.spec.hostRef == oldSelf.spec.hostRef",message="host identity is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.spec.model) == has(oldSelf.spec.model) && (!has(self.spec.model) || self.spec.model == oldSelf.spec.model)",message="resolved model is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.spec.shell) == has(oldSelf.spec.shell) && (!has(self.spec.shell) || self.spec.shell == oldSelf.spec.shell)",message="resolved shell is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.spec.training) == has(oldSelf.spec.training) && (!has(self.spec.training) || self.spec.training == oldSelf.spec.training)",message="resolved training run is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.spec.run) == has(oldSelf.spec.run) && (!has(self.spec.run) || self.spec.run == oldSelf.spec.run)",message="resolved run identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.spec.executionID == oldSelf.spec.executionID",message="execution identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.spec.fencingEpoch == oldSelf.spec.fencingEpoch",message="fencing epoch is immutable"
// +kubebuilder:validation:XValidation:rule="self.spec.leaseDurationSeconds == oldSelf.spec.leaseDurationSeconds",message="lease duration is immutable"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'active'",message="assignment mailbox object must be named active"
// +kubebuilder:validation:XValidation:rule="size(self.spec.workloadRef.uid) > 0 && size(self.spec.hostRef.uid) > 0",message="workload and host UIDs are required"
// +kubebuilder:validation:XValidation:rule="(has(self.spec.model) && !has(self.spec.shell) && !has(self.spec.training)) || (!has(self.spec.model) && has(self.spec.shell) && !has(self.spec.training)) || (!has(self.spec.model) && !has(self.spec.shell) && has(self.spec.training))",message="exactly one resolved model, shell, or training run is required"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.model) || (size(self.spec.model.catalogRef.uid) > 0 && quantity(self.spec.model.unifiedMemoryRequest).isGreaterThan(quantity('0')))",message="resolved model identity and memory request are required"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.shell) || quantity(self.spec.shell.unifiedMemoryRequest).isGreaterThan(quantity('0'))",message="resolved shell memory request must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.training) || quantity(self.spec.training.unifiedMemoryRequest).isGreaterThan(quantity('0'))",message="resolved training memory request must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.training) || (has(self.spec.run) && self.spec.run.task == 'train')",message="resolved training requires train run identity"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.run) || (has(self.spec.model) && has(self.spec.model.server) && self.spec.run.task == 'serve') || (has(self.spec.model) && has(self.spec.model.batch) && self.spec.run.task == 'infer') || (has(self.spec.shell) && self.spec.run.task == 'shell') || (has(self.spec.training) && self.spec.run.task == 'train')",message="resolved run task must match the execution intent"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.run) || !has(self.spec.run.parameters) || self.spec.run.parameters.all(k, k.matches('^[A-Z][A-Z0-9_]{0,62}$') && !k.startsWith('IDLELOOM_'))",message="resolved run parameter names must be safe environment names and must not use IDLELOOM_"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.shell) || self.spec.shell.isolation != 'Host' || self.spec.shell.network == 'Outbound'",message="Host shell isolation requires outbound network access"
type IdleloomWorkloadAssignment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IdleloomWorkloadAssignmentSpec   `json:"spec"`
	Status            IdleloomWorkloadAssignmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type IdleloomWorkloadAssignmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IdleloomWorkloadAssignment `json:"items"`
}

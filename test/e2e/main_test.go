//go:build e2e

// Package e2e deploys the downscaler with its Helm chart onto a local cluster,
// next to real operators, and checks downscaling and upscaling end to
// end. It only runs with the e2e build tag:
//
//	go test -tags e2e -v -timeout 60m ./test/e2e/...
//
// The cluster is the current kubeconfig context, which must be a local one
// (docker-desktop or kind-*). Environment:
//
//	E2E_SKIP_SETUP=1          reuse what a previous run installed
//	E2E_TEARDOWN=1            remove everything the run installed afterwards
//	E2E_DOWNSCALER_LOGS=info  stream the downscaler's logs while the tests run:
//	                          "info" for scale actions, warnings and errors,
//	                          "debug" for every line
//	E2E_LOG_FILE=path         where to write a copy of all output, with a time on
//	                          every line (default test/e2e/e2e.log; the run
//	                          before is kept as e2e.prev.log)
//
// The setup matches deployments/local/README.md, so a cluster set up by hand
// from the README can be tested with E2E_SKIP_SETUP=1.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	envNamespace        = "downscaler-e2e"
	downscalerNamespace = "downscaler"
	release             = "gkd"
	deploymentName      = "gkd-go-kube-downscaler"
	scanInterval        = 10 * time.Second

	// Keep these in step with deployments/local/README.md.
	certManagerVersion = "v1.21.3"
	rabbitmqVersion    = "v2.23.3"
	eckVersion         = "3.5.0"
	redisVersion       = "0.27.0"
	cnpgVersion        = "0.29.1"
	mongodbVersion     = "0.13.0"
)

// resources is what the downscaler manages in the tests: the example
// definitions, the built-in operator kinds, and plain Deployments and
// StatefulSets, so operator-owned StatefulSets are seen by both scalers.
var resources = []string{
	"deployments", "statefulsets", "cnpgclusters", "mongodbcommunities",
	"rabbitmqclusters", "elasticsearches", "redisreplications", "redissentinels",
}

var localContext = regexp.MustCompile(`^(docker-desktop|kind-.+)$`)

// cluster holds the clients and the kubeconfig context every command is pinned to.
type cluster struct {
	context string
	kube    kubernetes.Interface
	client  ctrlclient.Client
}

var (
	k8s      *cluster
	repoRoot string
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	repoRoot = filepath.Join(filepath.Dir(file), "..", "..")

	stopLog := teeOutput(logFilePath())

	code := run(m)

	progress("finished with exit code %d", code)
	stopLog()
	os.Exit(code)
}

func run(m *testing.M) int {
	var err error

	k8s, err = connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}

	if os.Getenv("E2E_SKIP_SETUP") != "1" {
		if err := setUp(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "e2e setup:", err)
			return 1
		}
	}

	logsCtx, stopLogs := context.WithCancel(context.Background())
	defer stopLogs()

	if setting := os.Getenv("E2E_DOWNSCALER_LOGS"); setting != "" {
		go followDownscalerLogs(logsCtx, setting)
	}

	code := m.Run()

	stopLogs()

	if os.Getenv("E2E_TEARDOWN") == "1" {
		tearDown()
	}

	return code
}

var errNotLocal = errors.New("refusing to run against a context that is not a local cluster")

// connect builds clients for the current kubeconfig context, after checking it
// is a local cluster.
func connect() (*cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()

	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	if !localContext.MatchString(raw.CurrentContext) {
		return nil, fmt.Errorf("%w: %q (want docker-desktop or kind-*)", errNotLocal, raw.CurrentContext)
	}

	config, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("client config: %w", err)
	}

	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	client, err := ctrlclient.New(config, ctrlclient.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("controller-runtime client: %w", err)
	}

	return &cluster{context: raw.CurrentContext, kube: kube, client: client}, nil
}

// setUp installs cert-manager, the five operators, the sample workloads and the
// downscaler built from this checkout. Every step can be repeated.
func setUp(ctx context.Context) error {
	steps := []struct {
		name string
		run  func() error
	}{
		{"helm repositories", helmRepos},
		{"cert-manager", func() error {
			return helmInstall("cert-manager", "jetstack/cert-manager", certManagerVersion, "cert-manager",
				"--set", "crds.enabled=true", "--wait")
		}},
		{"data namespace", func() error {
			return kubectlApplyStdin(fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", envNamespace))
		}},
		{"RabbitMQ operator", func() error {
			return kubectl("apply", "-f",
				"https://github.com/rabbitmq/cluster-operator/releases/download/"+rabbitmqVersion+"/cluster-operator.yml")
		}},
		{"ECK operator", func() error {
			return helmInstall("elastic-operator", "elastic/eck-operator", eckVersion, "elastic-system", "--wait")
		}},
		{"Redis operator", func() error {
			return helmInstall("redis-operator", "ot-helm/redis-operator", redisVersion, "redis-operator", "--wait")
		}},
		{"CloudNativePG operator", func() error {
			return helmInstall("cnpg", "cnpg/cloudnative-pg", cnpgVersion, "cnpg-system", "--wait")
		}},
		{"MongoDB Community operator", func() error {
			return helmInstall("community-operator", "mongodb/community-operator", mongodbVersion, "mongodb-operator",
				"--set", "operator.watchNamespace="+envNamespace, "--set", "database.namespace="+envNamespace, "--wait")
		}},
		{"operators ready", func() error {
			return firstError(
				kubectl("-n", "rabbitmq-system", "rollout", "status", "deploy/rabbitmq-cluster-operator", "--timeout=5m"),
				kubectl("-n", "elastic-system", "rollout", "status", "statefulset/elastic-operator", "--timeout=5m"),
			)
		}},
		{"sample workloads", func() error {
			return firstError(
				clearForceAnnotations(ctx),
				kubectl("apply", "-n", envNamespace, "-f", filepath.Join(repoRoot, "test", "e2e", "testdata", "environment.yaml")),
			)
		}},
		{"downscaler", deployDownscaler},
	}

	setupStart := time.Now()

	for i, step := range steps {
		progress("setup [%d/%d] %s", i+1, len(steps), step.name)

		start := time.Now()

		if err := step.run(); err != nil {
			progress("setup [%d/%d] %s FAILED after %s", i+1, len(steps), step.name, since(start))
			return fmt.Errorf("%s: %w", step.name, err)
		}

		progress("setup [%d/%d] %s done in %s", i+1, len(steps), step.name, since(start))
	}

	progress("setup complete in %s", since(setupStart))

	return nil
}

// progress prints a timestamped line to stderr, which go test shows as it happens.
func progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s e2e: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// since is the time elapsed since start, rounded for reading.
func since(start time.Time) time.Duration {
	return time.Since(start).Round(time.Second)
}

// deployDownscaler builds the controller image into the local Docker image
// store, which Docker Desktop's cluster pulls from directly, and installs the
// chart from this checkout with the example workload definitions.
func deployDownscaler() error {
	tag := fmt.Sprintf("e2e-%d", time.Now().Unix())
	image := "gokubedownscaler"

	if err := command("docker", "build", "--progress=plain", "-f", filepath.Join(repoRoot, "build", "kubedownscaler", "Dockerfile"),
		"-t", image+":"+tag, repoRoot); err != nil {
		return err
	}

	if strings.HasPrefix(k8s.context, "kind-") {
		if err := command("kind", "load", "docker-image", image+":"+tag,
			"--name", strings.TrimPrefix(k8s.context, "kind-")); err != nil {
			return err
		}
	}

	return helm("upgrade", "--install", release, filepath.Join(repoRoot, "deployments", "chart"),
		"-n", downscalerNamespace, "--create-namespace",
		"-f", filepath.Join(repoRoot, "deployments", "chart", "examples", "operator-workload-definitions.yaml"),
		"--set", "includedResources={"+strings.Join(resources, ",")+"}",
		"--set", "constrainedNamespaces={"+envNamespace+"}",
		"--set", "image.repository="+image, "--set", "image.tag="+tag,
		// No grace period: the workloads are only minutes old when the tests
		// force downtime, and the default 15 minutes would hold every one back.
		"--set", "extraArguments={--interval="+scanInterval.String()+",--debug,--grace-period=0}",
		"--wait", "--timeout=5m")
}

func tearDown() {
	progress("teardown")

	for _, args := range [][]string{
		{"delete", "namespace", envNamespace, "--wait=true", "--timeout=10m"},
	} {
		_ = kubectl(args...)
	}

	for _, r := range []struct{ release, namespace string }{
		{release, downscalerNamespace},
		{"community-operator", "mongodb-operator"},
		{"cnpg", "cnpg-system"},
		{"redis-operator", "redis-operator"},
		{"elastic-operator", "elastic-system"},
		{"cert-manager", "cert-manager"},
	} {
		_ = helm("uninstall", r.release, "-n", r.namespace)
		_ = kubectl("delete", "namespace", r.namespace, "--wait=false")
	}

	_ = kubectl("delete", "-f",
		"https://github.com/rabbitmq/cluster-operator/releases/download/"+rabbitmqVersion+"/cluster-operator.yml")
}

func helmRepos() error {
	for _, repo := range [][2]string{
		{"jetstack", "https://charts.jetstack.io"},
		{"elastic", "https://helm.elastic.co"},
		{"ot-helm", "https://ot-container-kit.github.io/helm-charts"},
		{"cnpg", "https://cloudnative-pg.github.io/charts"},
		{"mongodb", "https://mongodb.github.io/helm-charts"},
	} {
		if err := helm("repo", "add", repo[0], repo[1], "--force-update"); err != nil {
			return err
		}
	}

	return helm("repo", "update")
}

func helmInstall(name, chart, version, namespace string, extra ...string) error {
	args := append([]string{"upgrade", "--install", name, chart, "--version", version, "-n", namespace, "--create-namespace"}, extra...)
	return helm(args...)
}

func helm(args ...string) error {
	if args[0] != "repo" {
		args = append([]string{"--kube-context", k8s.context}, args...)
	}

	return command("helm", args...)
}

func kubectl(args ...string) error {
	return command("kubectl", append([]string{"--context", k8s.context}, args...)...)
}

func kubectlApplyStdin(manifest string) error {
	cmd := exec.Command("kubectl", "--context", k8s.context, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)

	return runCommand(cmd)
}

func command(name string, args ...string) error {
	return runCommand(exec.Command(name, args...))
}

// runCommand runs cmd, streaming its output to stderr indented under the command
// line, and returns the output in the error when it fails.
func runCommand(cmd *exec.Cmd) error {
	var output bytes.Buffer

	fmt.Fprintf(os.Stderr, "         $ %s\n", strings.Join(cmd.Args, " "))

	live := &indentWriter{out: os.Stderr, prefix: "         | "}
	both := io.MultiWriter(&output, live)
	cmd.Stdout = both
	cmd.Stderr = both

	err := cmd.Run()
	live.flush()

	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(cmd.Args, " "), err, output.String())
	}

	return nil
}

// indentWriter writes each complete line it receives to out with a prefix.
type indentWriter struct {
	mu      sync.Mutex
	out     io.Writer
	prefix  string
	pending []byte
}

func (w *indentWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.pending = append(w.pending, p...)

	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}

		fmt.Fprintf(w.out, "%s%s\n", w.prefix, bytes.TrimRight(w.pending[:i], "\r"))
		w.pending = w.pending[i+1:]
	}

	return len(p), nil
}

// flush writes a last line that did not end in a newline.
func (w *indentWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.pending) > 0 {
		fmt.Fprintf(w.out, "%s%s\n", w.prefix, w.pending)
		w.pending = nil
	}
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// Custom resource kinds the tests read.
var (
	rabbitmqClusterGVK  = schema.GroupVersionKind{Group: "rabbitmq.com", Version: "v1beta1", Kind: "RabbitmqCluster"}
	elasticsearchGVK    = schema.GroupVersionKind{Group: "elasticsearch.k8s.elastic.co", Version: "v1", Kind: "Elasticsearch"}
	redisReplicationGVK = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "RedisReplication"}
	redisSentinelGVK    = schema.GroupVersionKind{Group: "redis.redis.opstreelabs.in", Version: "v1beta2", Kind: "RedisSentinel"}
	cnpgClusterGVK      = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	mongoDBCommunityGVK = schema.GroupVersionKind{Group: "mongodbcommunity.mongodb.com", Version: "v1", Kind: "MongoDBCommunity"}
)

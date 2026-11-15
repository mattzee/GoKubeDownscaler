//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// logLevels orders the downscaler's log levels, to filter the live stream.
var logLevels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

// followDownscalerLogs prints the downscaler's log lines to stderr as it writes
// them, until ctx ends. E2E_DOWNSCALER_LOGS picks the lowest level shown:
// "debug" shows everything, any other non-empty value shows info and above
// (every scale action, warnings and errors). It follows the newest pod across
// restarts and rollouts.
func followDownscalerLogs(ctx context.Context, setting string) {
	minLevel := logLevels["INFO"]
	if strings.EqualFold(setting, "debug") {
		minLevel = logLevels["DEBUG"]
	}

	progress("following the downscaler's logs (E2E_DOWNSCALER_LOGS=%s)", setting)

	var (
		pod   string
		since time.Time
	)

	for ctx.Err() == nil {
		current, err := newestDownscalerPod(ctx)
		if err != nil || current == "" {
			sleepCtx(ctx, 2*time.Second)
			continue
		}

		options := &corev1.PodLogOptions{Follow: true, Timestamps: true}

		if current == pod {
			// Same pod after a dropped stream: carry on from the last line seen.
			options.SinceTime = &metav1.Time{Time: since}
		} else {
			progress("downscaler logs: following pod %s", current)
			pod = current
		}

		last := streamLogs(ctx, current, options, minLevel)
		if !last.IsZero() {
			since = last.Add(time.Nanosecond)
		}

		sleepCtx(ctx, 2*time.Second)
	}
}

// streamLogs prints one pod's log stream until it ends, and returns the time of
// the last line it read.
func streamLogs(ctx context.Context, pod string, options *corev1.PodLogOptions, minLevel int) time.Time {
	stream, err := k8s.kube.CoreV1().Pods(downscalerNamespace).GetLogs(pod, options).Stream(ctx)
	if err != nil {
		return time.Time{}
	}
	defer stream.Close()

	var last time.Time

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		// With Timestamps, each line starts with an RFC3339Nano time and a space.
		stamp, line, _ := strings.Cut(scanner.Text(), " ")
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			last = t
		} else {
			line, last = scanner.Text(), time.Now()
		}

		if formatted, show := formatLogLine(line, minLevel); show {
			fmt.Fprintf(os.Stderr, "%s downscaler: %s\n", last.Local().Format("15:04:05"), formatted)
		}
	}

	return last
}

// formatLogLine turns one JSON log line into "LEVEL message key=value ...",
// with the workload first, and reports whether it meets minLevel. A line that
// is not JSON is shown as it is.
func formatLogLine(line string, minLevel int) (string, bool) {
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return line, true
	}

	level := strings.ToUpper(fmt.Sprint(entry["severity"]))
	if rank, known := logLevels[level]; known && rank < minLevel {
		return "", false
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%-5s %v", level, entry["msg"])

	if kind, ok := entry["kind"]; ok {
		fmt.Fprintf(&b, "  %v %v/%v", kind, entry["namespace"], entry["workload"])
	}

	keys := make([]string, 0, len(entry))

	for key := range entry {
		switch key {
		case "time", "severity", "msg", "kind", "namespace", "workload":
		default:
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	for _, key := range keys {
		fmt.Fprintf(&b, " %s=%v", key, entry[key])
	}

	return b.String(), true
}

// newestDownscalerPod is the most recently created downscaler pod, or "" if none.
func newestDownscalerPod(ctx context.Context) (string, error) {
	pods, err := k8s.kube.CoreV1().Pods(downscalerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + release,
	})
	if err != nil {
		return "", fmt.Errorf("list downscaler pods: %w", err)
	}

	var (
		newest  string
		created time.Time
	)

	for _, pod := range pods.Items {
		if !strings.HasPrefix(pod.Name, deploymentName) || pod.Status.Phase == corev1.PodPending {
			continue
		}

		if pod.CreationTimestamp.After(created) {
			newest, created = pod.Name, pod.CreationTimestamp.Time
		}
	}

	return newest, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

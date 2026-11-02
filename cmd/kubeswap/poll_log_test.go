package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestKubeswap_WaitForDeploymentGone_PromptOnCancel verifies waitForDeploymentGone returns promptly when ctx is cancelled.
func TestKubeswap_WaitForDeploymentGone_PromptOnCancel(t *testing.T) {
	client := newFakeClient()
	stuck := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "stuck", Namespace: "llama-swap"}}
	if _, err := client.AppsV1().Deployments("llama-swap").Create(context.Background(), stuck, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := waitForDeploymentGone(ctx, client, "llama-swap", "stuck", 10*time.Second)
	if time.Since(start) > 2*time.Second {
		t.Errorf("waitForDeploymentGone took %v, want under 2s", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want context canceled error, got %v", err)
	}
}

// TestKubeswap_WaitForPodsGone_PromptOnCancel verifies waitForPodsGone returns promptly when ctx is cancelled.
func TestKubeswap_WaitForPodsGone_PromptOnCancel(t *testing.T) {
	client := newFakeClient()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "p1",
		Namespace: "llama-swap",
		Labels:    podSelectorLabels("m", "dep-m"),
	}}
	if _, err := client.CoreV1().Pods("llama-swap").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := waitForPodsGone(ctx, client, "llama-swap", "m", "dep-m", 10*time.Second)
	if time.Since(start) > 2*time.Second {
		t.Errorf("waitForPodsGone took %v, want under 2s", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want context canceled error, got %v", err)
	}
}

// TestKubeswap_ReadLogStream_EscapesLines verifies log forwarding escapes CR and LF characters in forwarded lines.
func TestKubeswap_ReadLogStream_EscapesLines(t *testing.T) {
	var buf bytes.Buffer
	s := &server{logWriter: &buf}
	input := "line1\rwith_cr\nline2\na\rb\n"
	s.readLogStream(context.Background(), io.NopCloser(strings.NewReader(input)), "pod-1")
	got := buf.String()
	want := "[pod/pod-1] line1\\rwith_cr\n[pod/pod-1] line2\n[pod/pod-1] a\\rb\n"
	if got != want {
		t.Errorf("readLogStream output = %q, want %q", got, want)
	}
}

// TestKubeswap_ReadLogStream_ErrorReported verifies that a reader returning an error reports it with logf.
func TestKubeswap_ReadLogStream_ErrorReported(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	s := &server{}
	s.readLogStream(context.Background(), io.NopCloser(iotest.ErrReader(errors.New("simulated error"))), "pod-err")
	got := logBuf.String()
	if !strings.Contains(got, "reading log stream for pod pod-err: simulated error") {
		t.Fatalf("expected error logged, got %q", got)
	}
}

// TestKubeswap_ReadLogStream_CancelledContextSuppressesError verifies no error is logged if context is cancelled.
func TestKubeswap_ReadLogStream_CancelledContextSuppressesError(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &server{}
	s.readLogStream(ctx, io.NopCloser(iotest.ErrReader(errors.New("simulated error"))), "pod-err")
	if logBuf.Len() > 0 {
		t.Fatalf("expected no error logged on cancelled context, got %q", logBuf.String())
	}
}

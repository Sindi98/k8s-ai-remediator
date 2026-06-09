package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newConfigServer(cs *fake.Clientset) *Server {
	return &Server{
		opts: Options{
			Namespace:      "ns",
			ConfigMapName:  "cfg",
			SecretName:     "sec",
			DeploymentName: "dep",
			Clientset:      cs,
		},
	}
}

func TestPatchConfigMap_CreatesWhenMissing(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := newConfigServer(cs)
	if err := s.patchConfigMap(context.Background(), map[string]string{"A": "1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "cfg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("configmap not created: %v", err)
	}
	if cm.Data["A"] != "1" {
		t.Errorf("want A=1, got %q", cm.Data["A"])
	}
}

func TestPatchConfigMap_MergesExisting(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "ns"},
		Data:       map[string]string{"A": "old", "B": "keep"},
	}
	cs := fake.NewSimpleClientset(cm)
	s := newConfigServer(cs)
	if err := s.patchConfigMap(context.Background(), map[string]string{"A": "new", "C": "add"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "cfg", metav1.GetOptions{})
	if got.Data["A"] != "new" || got.Data["B"] != "keep" || got.Data["C"] != "add" {
		t.Errorf("merge wrong: %+v", got.Data)
	}
}

func TestPatchSecret_CreatesAndMerges(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := newConfigServer(cs)
	if err := s.patchSecret(context.Background(), map[string][]byte{"P": []byte("x")}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.patchSecret(context.Background(), map[string][]byte{"Q": []byte("y")}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	sec, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "sec", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret not present: %v", err)
	}
	if string(sec.Data["P"]) != "x" || string(sec.Data["Q"]) != "y" {
		t.Errorf("secret merge wrong: P=%q Q=%q", sec.Data["P"], sec.Data["Q"])
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("expected Opaque secret, got %s", sec.Type)
	}
}

func TestValidateConfigValue(t *testing.T) {
	ok := []struct {
		kind configKind
		raw  string
	}{
		{kindInt, "30"},
		{kindFloat, "2.5"},
		{kindUnitFloat, "0.85"},
		{kindBool, "true"},
		{kindSeverity, "high"},
		{kindEnum, "redis"},
		{kindString, "anything"},
		{kindCSV, "a,b,c"},
	}
	for _, c := range ok {
		if err := validateConfigValue(c.kind, c.raw); err != nil {
			t.Errorf("validateConfigValue(%d, %q) unexpected error: %v", c.kind, c.raw, err)
		}
	}
	bad := []struct {
		kind configKind
		raw  string
	}{
		{kindInt, "abc"},
		{kindUnitFloat, "2"},     // out of [0,1]
		{kindUnitFloat, "x"},     // not a number
		{kindBool, "maybe"},      //
		{kindSeverity, "urgent"}, // not a known severity
		{kindEnum, "sqlite"},     // not memory|redis
	}
	for _, c := range bad {
		if err := validateConfigValue(c.kind, c.raw); err == nil {
			t.Errorf("validateConfigValue(%d, %q) expected error", c.kind, c.raw)
		}
	}
}

func TestHandleUpdateGeneral_PersistsAndRollsOut(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(dep)
	s := newConfigServer(cs)

	form := url.Values{}
	form.Set("MIN_SEVERITY", "high")
	form.Set("DRY_RUN", "true")
	form.Set("__bool_fields", "DRY_RUN")
	req := httptest.NewRequest(http.MethodPost, "/api/config/general", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleUpdateGeneral(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	cm, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "cfg", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("configmap not written: %v", err)
	}
	if cm.Data["MIN_SEVERITY"] != "high" || cm.Data["DRY_RUN"] != "true" {
		t.Errorf("unexpected config: %+v", cm.Data)
	}

	// Rollout annotation bumped on the Deployment template.
	got, _ := cs.AppsV1().Deployments("ns").Get(context.Background(), "dep", metav1.GetOptions{})
	if _, ok := got.Spec.Template.Annotations["webui/config-revision"]; !ok {
		t.Error("expected rollout annotation on the Deployment template")
	}
}

func TestHandleUpdateGeneral_RejectsInvalidValue(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := newConfigServer(cs)

	form := url.Values{}
	form.Set("PATCH_CONFIDENCE_THRESHOLD", "2") // out of [0,1]
	req := httptest.NewRequest(http.MethodPost, "/api/config/general", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	s.handleUpdateGeneral(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid value, got %d: %s", rec.Code, rec.Body.String())
	}
	// Nothing should have been persisted.
	if _, err := cs.CoreV1().ConfigMaps("ns").Get(context.Background(), "cfg", metav1.GetOptions{}); err == nil {
		t.Error("configmap should not be created when validation fails")
	}
}

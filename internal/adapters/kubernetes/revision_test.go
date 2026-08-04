package kubernetes

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTemplateRevisionIsDeterministicAndIgnoresControllerHash(t *testing.T) {
	base := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: "demo:v1"}}},
	}
	withHash := base.DeepCopy()
	withHash.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "generated"

	first, err := TemplateRevision(base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TemplateRevision(*withHash)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("controller hash changed revision: %q != %q", first, second)
	}
	if !strings.HasPrefix(string(first), "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("revision = %q, want sha256 digest", first)
	}
	if withHash.Labels[appsv1.DefaultDeploymentUniqueLabelKey] != "generated" {
		t.Fatal("TemplateRevision mutated its input")
	}

	changed := base.DeepCopy()
	changed.Spec.Containers[0].Image = "demo:v2"
	third, err := TemplateRevision(*changed)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("meaningful template change did not change revision")
	}
}

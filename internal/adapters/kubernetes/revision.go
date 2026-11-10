package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/belLena81/rollback-guard/internal/domain"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const StableRevisionAnnotation = "rollback-guardian.io/stable"

// stableAnnotationValue marks a ReplicaSet as the previous stable revision.
const stableAnnotationValue = "true"

func TemplateRevision(template corev1.PodTemplateSpec) (domain.Revision, error) {
	normalized := normalizeTemplate(template)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return domain.Revision("sha256:" + hex.EncodeToString(sum[:])), nil
}

func normalizeTemplate(template corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	normalized := *template.DeepCopy()
	delete(normalized.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if len(normalized.Labels) == 0 {
		normalized.Labels = nil
	}
	if len(normalized.Annotations) == 0 {
		normalized.Annotations = nil
	}
	return normalized
}

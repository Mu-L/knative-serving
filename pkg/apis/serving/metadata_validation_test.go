/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package serving

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"
	"knative.dev/serving/pkg/apis/autoscaling"
	"knative.dev/serving/pkg/apis/config"
	"knative.dev/serving/pkg/autoscaler/config/autoscalerconfig"
)

func TestValidateObjectMetadata(t *testing.T) {
	cases := []struct {
		name             string
		objectMeta       metav1.Object
		allowAutoscaling bool
		ctx              context.Context
		expectErr        *apis.FieldError
	}{{
		name: "invalid name - dots",
		objectMeta: &metav1.ObjectMeta{
			Name: "do.not.use.dots",
		},
		expectErr: &apis.FieldError{
			Message: "not a DNS 1035 label: [a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character (e.g. 'my-name',  or 'abc-123', regex used for validation is '[a-z]([-a-z0-9]*[a-z0-9])?')]",
			Paths:   []string{"name"},
		},
	}, {
		name: "invalid name - too long",
		objectMeta: &metav1.ObjectMeta{
			Name: strings.Repeat("a", 64),
		},
		expectErr: &apis.FieldError{
			Message: "not a DNS 1035 label: [must be no more than 63 characters]",
			Paths:   []string{"name"},
		},
	}, {
		name: "invalid name - trailing dash",
		objectMeta: &metav1.ObjectMeta{
			Name: "some-name-",
		},
		expectErr: &apis.FieldError{
			Message: "not a DNS 1035 label: [a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character (e.g. 'my-name',  or 'abc-123', regex used for validation is '[a-z]([-a-z0-9]*[a-z0-9])?')]",
			Paths:   []string{"name"},
		},
	}, {
		name: "valid generateName",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
		},
	}, {
		name: "valid generateName - trailing dash",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name-",
		},
	}, {
		name: "invalid generateName - dots",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "do.not.use.dots",
		},
		expectErr: &apis.FieldError{
			Message: "not a DNS 1035 label prefix: [a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character (e.g. 'my-name',  or 'abc-123', regex used for validation is '[a-z]([-a-z0-9]*[a-z0-9])?')]",
			Paths:   []string{"generateName"},
		},
	}, {
		name: "invalid generateName - too long",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: strings.Repeat("a", 64),
		},
		expectErr: &apis.FieldError{
			Message: "not a DNS 1035 label prefix: [must be no more than 63 characters]",
			Paths:   []string{"generateName"},
		},
	}, {
		name:       "missing name and generateName",
		objectMeta: &metav1.ObjectMeta{},
		expectErr: &apis.FieldError{
			Message: "name or generateName is required",
			Paths:   []string{"name"},
		},
	}, {
		name: "valid creator annotation label",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				CreatorAnnotation: "svc-creator",
			},
		},
	}, {
		name: "valid lastModifier annotation label",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				UpdaterAnnotation: "svc-modifier",
			},
		},
	}, {
		name: "valid lastPinned annotation label",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				RevisionLastPinnedAnnotationKey: "pinned-val",
			},
		},
	}, {
		name: "valid preserve annotation label",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				RevisionLastPinnedAnnotationKey: "true",
			},
		},
	}, {
		name: "valid knative prefix annotation",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				"serving.knative.dev/testAnnotation": "value",
			},
		},
	}, {
		name: "valid non-knative prefix annotation label",
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				"testAnnotation": "testValue",
			},
		},
	}, {
		name:             "revision initial scale not parseable",
		ctx:              config.ToContext(context.Background(), &config.Config{Autoscaler: &autoscalerconfig.Config{AllowZeroInitialScale: true}}),
		allowAutoscaling: true,
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				autoscaling.InitialScaleAnnotationKey: "invalid",
			},
		},
		expectErr: apis.ErrInvalidValue("invalid", "annotations."+autoscaling.InitialScaleAnnotationKey),
	}, {
		name:             "negative revision initial scale",
		ctx:              config.ToContext(context.Background(), &config.Config{Autoscaler: &autoscalerconfig.Config{AllowZeroInitialScale: true}}),
		allowAutoscaling: true,
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				autoscaling.InitialScaleAnnotationKey: "-2",
			},
		},
		expectErr: apis.ErrInvalidValue("-2", "annotations."+autoscaling.InitialScaleAnnotationKey+" must be greater than 0"),
	}, {
		name:             "cluster allows zero revision initial scale",
		ctx:              config.ToContext(context.Background(), &config.Config{Autoscaler: &autoscalerconfig.Config{AllowZeroInitialScale: true}}),
		allowAutoscaling: true,
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				autoscaling.InitialScaleAnnotationKey: "0",
			},
		},
	}, {
		name:             "cluster does not allow zero revision initial scale",
		allowAutoscaling: true,
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				autoscaling.InitialScaleAnnotationKey: "0",
			},
		},
		expectErr: apis.ErrInvalidValue("0", "annotations."+autoscaling.InitialScaleAnnotationKey+"=0 not allowed by cluster"),
	}, {
		name:             "autoscaling annotations on a resource that doesn't allow them",
		allowAutoscaling: false,
		objectMeta: &metav1.ObjectMeta{
			GenerateName: "some-name",
			Annotations: map[string]string{
				autoscaling.InitialScaleAnnotationKey: "0",
			},
		},
		expectErr: apis.ErrInvalidKeyName(autoscaling.InitialScaleAnnotationKey, "annotations", `autoscaling annotations must be put under "spec.template.metadata.annotations" to work`),
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.ctx == nil {
				//nolint:fatcontext
				c.ctx = config.ToContext(context.Background(), &config.Config{Autoscaler: &autoscalerconfig.Config{AllowZeroInitialScale: false}})
			}
			err := ValidateObjectMetadata(c.ctx, c.objectMeta, c.allowAutoscaling)
			if got, want := err.Error(), c.expectErr.Error(); got != want {
				t.Errorf("\nGot:  %q\nwant: %q", got, want)
			}
		})
	}
}

func TestValidateHasNoAutoscalingAnnotation(t *testing.T) {
	cases := []struct {
		name       string
		annotation map[string]string
		expectErr  *apis.FieldError
	}{{
		name:       "nil",
		annotation: nil,
	}, {
		name:       "empty",
		annotation: map[string]string{},
	}, {
		name:       "no offender",
		annotation: map[string]string{"foo": "bar"},
	}, {
		name:       "only offender",
		annotation: map[string]string{"autoscaling.knative.dev/foo": "bar"},
		expectErr:  apis.ErrInvalidKeyName("autoscaling.knative.dev/foo", apis.CurrentField, `autoscaling annotations must be put under "spec.template.metadata.annotations" to work`),
	}, {
		name: "offender and non-offender",
		annotation: map[string]string{
			"autoscaling.knative.dev/foo": "bar",
			"foo":                         "bar",
		},
		expectErr: apis.ErrInvalidKeyName("autoscaling.knative.dev/foo", apis.CurrentField, `autoscaling annotations must be put under "spec.template.metadata.annotations" to work`),
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateHasNoAutoscalingAnnotation(c.annotation)
			if got, want := err.Error(), c.expectErr.Error(); got != want {
				t.Errorf("\nGot:  %q\nwant: %q", got, want)
			}
		})
	}
}

func cfg(m map[string]string) *config.Config {
	d, _ := config.NewDefaultsConfigFromMap(m)
	return &config.Config{
		Defaults: d,
	}
}

func TestValidateContainerConcurrency(t *testing.T) {
	cases := []struct {
		name                 string
		containerConcurrency *int64
		ctx                  context.Context
		expectErr            *apis.FieldError
	}{{
		name:                 "empty containerConcurrency",
		containerConcurrency: nil,
	}, {
		name:                 "invalid containerConcurrency value",
		containerConcurrency: ptr.Int64(2000),
		expectErr: apis.ErrOutOfBoundsValue(
			2000, 0, config.DefaultMaxRevisionContainerConcurrency, apis.CurrentField),
	}, {
		name:                 "invalid containerConcurrency value, non def config",
		containerConcurrency: ptr.Int64(2000),
		ctx: config.ToContext(context.Background(), cfg(map[string]string{
			"container-concurrency-max-limit": "1950",
		})),
		expectErr: apis.ErrOutOfBoundsValue(
			2000, 0, 1950, apis.CurrentField),
	}, {
		name:                 "invalid containerConcurrency value, zero but allow-container-concurrency-zero is false",
		containerConcurrency: ptr.Int64(0),
		ctx: config.ToContext(context.Background(), cfg(map[string]string{
			"allow-container-concurrency-zero": "false",
		})),
		expectErr: apis.ErrOutOfBoundsValue(
			0, 1, config.DefaultMaxRevisionContainerConcurrency, apis.CurrentField),
	}, {
		name:                 "valid containerConcurrency value",
		containerConcurrency: ptr.Int64(10),
	}, {
		name:                 "valid containerConcurrency value zero",
		containerConcurrency: ptr.Int64(0),
	}, {
		name:                 "valid containerConcurrency value huge",
		containerConcurrency: ptr.Int64(2019),
		ctx: config.ToContext(context.Background(), cfg(map[string]string{
			"container-concurrency-max-limit": "2021",
		})),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ctx == nil {
				tc.ctx = context.Background()
			}
			err := ValidateContainerConcurrency(tc.ctx, tc.containerConcurrency)
			if got, want := err.Error(), tc.expectErr.Error(); got != want {
				t.Errorf("\nGot:  %q\nwant: %q", got, want)
			}
		})
	}
}

type withPod struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              corev1.PodSpec `json:"spec,omitempty"`
}

func getSpec(image string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Image: image,
		}},
	}
}

func TestAnnotationCreate(t *testing.T) {
	const (
		u1 = "oveja@knative.dev"
		u2 = "cabra@knative.dev"
	)
	tests := []struct {
		name string
		user string
		this *withPod
		want map[string]string
	}{{
		name: "create annotation",
		user: u1,
		this: &withPod{
			Spec: getSpec("foo"),
		},
		want: map[string]string{
			CreatorAnnotation: u1,
			UpdaterAnnotation: u1,
		},
	}, {
		name: "create annotation should override user provided annotations",
		user: u1,
		this: &withPod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					CreatorAnnotation: u2,
					UpdaterAnnotation: u2,
				},
			},
			Spec: getSpec("foo"),
		},
		want: map[string]string{
			CreatorAnnotation: u1,
			UpdaterAnnotation: u1,
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := apis.WithUserInfo(context.Background(), &authv1.UserInfo{
				Username: test.user,
			})
			SetUserInfo(ctx, nil, test.this.Spec, test.this)
			if !cmp.Equal(test.this.Annotations, test.want) {
				t.Errorf("Annotations = %v, want: %v, diff (-want, +got):\n%s", test.this.Annotations, test.want,
					cmp.Diff(test.want, test.this.Annotations))
			}
		})
	}
}

func TestAnnotationUpdate(t *testing.T) {
	const (
		u1 = "oveja@knative.dev"
		u2 = "cabra@knative.dev"
	)
	tests := []struct {
		name string
		user string
		prev *withPod
		this *withPod
		want map[string]string
	}{{
		name: "update annotation without spec changes",
		user: u2,
		this: &withPod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					CreatorAnnotation: u1,
					UpdaterAnnotation: u1,
				},
			},
			Spec: getSpec("foo"),
		},
		prev: &withPod{
			Spec: getSpec("foo"),
		},
		want: map[string]string{
			CreatorAnnotation: u1,
			UpdaterAnnotation: u1,
		},
	}, {
		name: "update annotation with spec changes",
		user: u2,
		this: &withPod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					CreatorAnnotation: u1,
					UpdaterAnnotation: u1,
				},
			},
			Spec: getSpec("bar"),
		},
		prev: &withPod{
			Spec: getSpec("foo"),
		},
		want: map[string]string{
			CreatorAnnotation: u1,
			UpdaterAnnotation: u2,
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := apis.WithUserInfo(context.Background(), &authv1.UserInfo{
				Username: test.user,
			})
			if test.prev != nil {
				ctx = apis.WithinUpdate(ctx, test.prev)
			}
			SetUserInfo(ctx, test.prev.Spec, test.this.Spec, test.this)
			if !cmp.Equal(test.this.Annotations, test.want) {
				t.Errorf("Annotations = %v, want: %v, diff (-want, +got):\n%s", test.this.Annotations, test.want,
					cmp.Diff(test.want, test.this.Annotations))
			}
		})
	}
}

func TestValidateRolloutDurationAnnotation(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{{
		name: "empty",
	}, {
		name:  "valid",
		value: "120s",
	}, {
		name:  "fancy valid",
		value: "3h15m21s",
	}, {
		name:  "in ns",
		value: "120000000000",
		want:  "invalid value: 120000000000: serving.knative.dev/rollout-duration",
	}, {
		name:  "not a valid duration",
		value: "five minutes and 6 seconds",
		want:  "invalid value: five minutes and 6 seconds: serving.knative.dev/rollout-duration",
	}, {
		name:  "negative",
		value: "-211s",
		want:  "rollout-duration=-211s must be positive: serving.knative.dev/rollout-duration",
	}, {
		name:  "too precise",
		value: "211s44ms",
		want:  "rollout-duration=211s44ms is not at second precision: serving.knative.dev/rollout-duration",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRolloutDurationAnnotation(map[string]string{
				RolloutDurationKey: tc.value,
			})
			if got, want := err.Error(), tc.want; got != want {
				t.Errorf("APIErr mismatch, diff(-want,+got):\n%s", cmp.Diff(want, got))
			}
		})
	}
}

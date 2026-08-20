package controller

import (
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"
)

func ptr32(v int32) *int32 { return &v }

func TestValidateSpec_Autoscaling(t *testing.T) {
	cases := []struct {
		name    string
		as      appsv1alpha1.AutoscalingSpec
		wantErr bool
	}{
		{
			name:    "disabled ignores missing maxReplicas",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: false},
			wantErr: false,
		},
		{
			name:    "enabled without maxReplicas is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true},
			wantErr: true,
		},
		{
			name:    "enabled with maxReplicas=0 is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 0},
			wantErr: true,
		},
		{
			name:    "enabled with valid maxReplicas is accepted",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 10},
			wantErr: false,
		},
		{
			name:    "minReplicas exceeding maxReplicas is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: ptr32(11), MaxReplicas: 10},
			wantErr: true,
		},
		{
			name:    "minReplicas below 1 is rejected",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: ptr32(0), MaxReplicas: 10},
			wantErr: true,
		},
		{
			name:    "valid min/max bounds are accepted",
			as:      appsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: ptr32(3), MaxReplicas: 10},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &appsv1alpha1.JavaApplication{}
			app.Spec.Autoscaling = tc.as
			err := validateSpec(app)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

package render

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func Test_clusterMachineApprover(t *testing.T) {
	type args struct {
		images     map[string]string
		versions   map[string]string
		params     interface{}
		pullSecret []byte
		secrets    *corev1.SecretList
		configMaps *corev1.ConfigMapList
	}
	tests := []struct {
		name       string
		args       args
		wantErr    bool
		wantSubStr map[string]string
	}{
		{
			name: "4.10.0 cluster should use release images",
			args: args{
				images:   map[string]string{"cluster-machine-approver": "example/cluster-machine-approver:4.10"},
				versions: map[string]string{"release": "4.10.0"},
			},
			wantErr:    false,
			wantSubStr: map[string]string{"cluster-machine-approver-deployment.yaml": "image: example/cluster-machine-approver:4.10"},
		},
		{
			name: "4.9.2 cluster should use default images",
			args: args{
				images:   map[string]string{"cluster-machine-approver": "example/cluster-machine-approver:4.9"},
				versions: map[string]string{"release": "4.9.2"},
			},
			wantErr:    false,
			wantSubStr: map[string]string{"cluster-machine-approver-deployment.yaml": "image: quay.io/hanqiuzh/origin-cluster-machine-approver@sha256:ae555a2d6bbf99ea5ed0764216f06c759ac0fd861340afda8f714a29bd2da44b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newClusterManifestContext(tt.args.images, tt.args.versions, tt.args.params, tt.args.pullSecret, tt.args.secrets, tt.args.configMaps)
			ctx.clusterMachineApprover()
			res, err := ctx.renderManifests()
			if (err != nil) != tt.wantErr {
				t.Errorf("unexpected err returned. wantErr: %t. got: %v.", tt.wantErr, err)
				return
			}
			for k, v := range tt.wantSubStr {
				if !strings.Contains(string(res[k]), v) {
					t.Errorf("failed to detect substring in the rendered files. wantSubstr: %s. got: %s.", v, res[k])
				}
			}
		})
	}
}

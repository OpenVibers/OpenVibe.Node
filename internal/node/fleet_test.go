package node

import (
	"reflect"
	"testing"

	"github.com/OpenVibers/OpenVibe.Node/internal/config"
)

func TestCodesProbeCommand(t *testing.T) {
	entry := config.FunctionConfig{Name: "openvibe-codes", Version: "1.0.0", Class: "code",
		Command: []string{"/usr/bin/node", "/opt/openvibe-codes/node_modules/openvibe-codes/bin/openvibe-codes.js", "job"}}
	cases := []struct {
		name string
		fns  []config.FunctionConfig
		want []string
	}{
		{"the installer's entry, asked for logins", []config.FunctionConfig{entry},
			[]string{"/usr/bin/node", "/opt/openvibe-codes/node_modules/openvibe-codes/bin/openvibe-codes.js", "logins", "--json"}},
		{"an entry with no verb keeps its command and gets one", []config.FunctionConfig{{Name: "openvibe-codes", Class: "code", Command: []string{"/opt/codes/bin/run"}}},
			[]string{"/opt/codes/bin/run", "logins", "--json"}},
		{"no Codes runner", []config.FunctionConfig{{Name: "thumbnail", Class: "code", Command: []string{"/opt/fn/run"}}}, nil},
		{"another class is not the runner", []config.FunctionConfig{{Name: "openvibe-codes", Class: "function", Command: []string{"/opt/codes/run", "job"}}}, nil},
	}
	for _, c := range cases {
		if got := codesProbeCommand(c.fns); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

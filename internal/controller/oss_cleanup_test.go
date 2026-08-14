package controller

import (
	"reflect"
	"testing"
)

func TestWorkspaceOSSPrefix(t *testing.T) {
	tests := []struct {
		name   string
		wsName string
		want   string
	}{
		{name: "normal", wsName: "ws-aikc", want: "workspaces/ws-aikc"},
		{name: "with spaces", wsName: " ws-aikc ", want: "workspaces/ws-aikc"},
		{name: "empty", wsName: "", want: ""},
		{name: "slash in name", wsName: "ws/aikc", want: ""},
		{name: "path traversal", wsName: "../other", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workspaceOSSPrefix(tt.wsName); got != tt.want {
				t.Errorf("workspaceOSSPrefix(%q) = %q, want %q", tt.wsName, got, tt.want)
			}
		})
	}
}

func TestFilterWorkspaceKeys(t *testing.T) {
	prefix := "workspaces/ws-aikc"
	keys := []string{
		"workspaces/ws-aikc",                    // 目录占位对象，应删除
		"workspaces/ws-aikc/.bashrc",            // 属于本 workspace，应删除
		"workspaces/ws-aikc/data/model.bin",     // 属于本 workspace，应删除
		"workspaces/ws-aikc-dev/main.py",        // 同名前缀的其他 workspace，必须保留
		"workspaces/ws-aikc-dev",                // 同名前缀的其他 workspace 目录，必须保留
		"workspaces/ws-aikc-tmp/note.txt",       // 同名前缀的其他 workspace，必须保留
		"workspaces/ws-other/data.bin",          // 其他 workspace，必须保留
		"shared-assets/tools.tar.gz",            // 公共共享目录，必须保留
		"",                                      // 异常 key（理论不会出现），必须保留
	}
	want := []string{
		"workspaces/ws-aikc",
		"workspaces/ws-aikc/.bashrc",
		"workspaces/ws-aikc/data/model.bin",
	}
	if got := filterWorkspaceKeys(keys, prefix); !reflect.DeepEqual(got, want) {
		t.Errorf("filterWorkspaceKeys() = %v, want %v", got, want)
	}
}

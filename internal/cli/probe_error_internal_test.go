package cli

import (
	"errors"
	"strings"
	"testing"

	"lax/internal/gateway"
	"lax/internal/store"
)

// TestMergeProbeErrorKeepsSettledStatus 钉住包装层的规矩：结论已定就不改写状态。
// 具体一例是 401 之后停用账号失败：结论必须留在 auth，否则整批探活不会停。
func TestMergeProbeErrorKeepsSettledStatus(t *testing.T) {
	disableErr := errors.New("停用账号 ds1 失败，需手工处理: database is closed")

	t.Run("停用失败仍是 auth", func(t *testing.T) {
		in := gateway.ModelProbe{Model: "m1", Status: store.ModelAuthFailed, HTTPStatus: 401, Message: "invalid api key"}
		got := mergeProbeError("m1", in, disableErr)
		if got.Status != store.ModelAuthFailed {
			t.Fatalf("状态 = %q，期望 %q", got.Status, store.ModelAuthFailed)
		}
		if got.HTTPStatus != 401 {
			t.Fatalf("HTTP 状态被改动: %d", got.HTTPStatus)
		}
		if !strings.Contains(got.Message, "invalid api key") || !strings.Contains(got.Message, "停用") {
			t.Fatalf("备注应同时留下上游原话与停用失败原因: %q", got.Message)
		}
	})

	t.Run("有结论但没备注", func(t *testing.T) {
		in := gateway.ModelProbe{Model: "m1", Status: store.ModelForbidden, HTTPStatus: 403}
		got := mergeProbeError("m1", in, disableErr)
		if got.Status != store.ModelForbidden {
			t.Fatalf("状态 = %q，期望 %q", got.Status, store.ModelForbidden)
		}
		if got.Message != disableErr.Error() {
			t.Fatalf("备注 = %q，期望错误原文", got.Message)
		}
	})

	t.Run("没有结论时记成 error", func(t *testing.T) {
		got := mergeProbeError("m1", gateway.ModelProbe{Model: "m1"}, errors.New("解密凭据失败"))
		if got.Status != store.ModelError {
			t.Fatalf("状态 = %q，期望 %q", got.Status, store.ModelError)
		}
		if got.Model != "m1" {
			t.Fatalf("模型名 = %q", got.Model)
		}
		if got.Message != "解密凭据失败" {
			t.Fatalf("备注 = %q", got.Message)
		}
	})
}

package cli

import (
	"testing"

	"lax/internal/plugin"
)

// TestBuildInstallPlanDeclaredRoutes 钉住安装计划对 manifest 两条新声明的处理：
// 写了 routes 就原样落库（不再按方言推导端点），写了 auth 就带上凭据头两列。
func TestBuildInstallPlanDeclaredRoutes(t *testing.T) {
	pkg := plugin.Plugin{
		Manifest: plugin.Manifest{
			ID:      "com.example.voiceroutes",
			Name:    "VoiceRoutes",
			Version: "1.0.0",
			Kind:    plugin.KindProvider,
			Runtime: plugin.RuntimeDeclarative,
			Providers: []plugin.ProviderDecl{{
				ID:      "voiceroutes",
				Name:    "VoiceRoutes",
				BaseURL: "https://openspeech.example.test",
				Auth:    &plugin.AuthDecl{Header: "X-Api-Key", Scheme: "raw"},
				Routes: []plugin.RouteDecl{
					{Protocol: plugin.WireSpeech, Path: "/api/v3/tts/unidirectional"},
					{Protocol: plugin.WireTranscriptions, Path: "/api/v3/auc/bigmodel/recognize/flash"},
				},
			}},
		},
		Catalogs:           map[string]plugin.CatalogFile{},
		DialectsByProvider: map[string][]string{"voiceroutes": nil},
	}

	plan, catalogs := buildInstallPlan(pkg, plugin.TrustDeclarative)
	if len(plan.Providers) != 1 || len(catalogs) != 1 {
		t.Fatalf("计划 = %+v / 目录 = %+v", plan.Providers, catalogs)
	}
	if got := plan.Providers[0].Routes; len(got) != 2 ||
		got[0] != "openai-speech /api/v3/tts/unidirectional" ||
		got[1] != "openai-transcriptions /api/v3/auc/bigmodel/recognize/flash" {
		t.Fatalf("计划里的路由 = %v，期望按 manifest 声明原样保留", got)
	}
	if got := plan.Providers[0].Auth; got != "x-api-key raw" {
		t.Fatalf("计划里的凭据头 = %q，期望 x-api-key raw", got)
	}
	catalog := catalogs[0]
	if catalog.AuthHeader != "x-api-key" || catalog.AuthScheme != "raw" {
		t.Fatalf("目录里的凭据头 = %q/%q", catalog.AuthHeader, catalog.AuthScheme)
	}
	if len(catalog.Routes) != 2 || catalog.Routes[0].Protocol != plugin.WireSpeech {
		t.Fatalf("目录里的路由 = %+v", catalog.Routes)
	}
}

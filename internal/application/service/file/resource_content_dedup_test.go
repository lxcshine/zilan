package file

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// FR-18 规则 5：相同图片字节的重复出现复用同一资源句柄 —— SaveBytes 接线测试。
// 从上游 WeKnora-main internal/application/service/file/resource_content_dedup_test.go
// 照抄（catalogStub 上的 reused 字段为本次补齐）。

func TestSaveBytesDeduplicatesImagePayloads(t *testing.T) {
	catalog := &catalogStub{reused: "resource://ExistingHandle"}
	svc := NewResourceCatalogFileService(&physicalFileStub{}, catalog)

	ref, err := svc.SaveBytes(context.Background(), []byte("image"), 7, "a.png", false)
	require.NoError(t, err)
	require.Equal(t, "resource://ExistingHandle", ref)
}

func TestSaveBytesKeepsWritingWhenDeduplicationIsUncertain(t *testing.T) {
	cases := map[string]struct {
		name   string
		reused string
	}{
		"non-image kind":  {"export.pdf", ""},
		"temporary":       {"a.png", "resource://ExistingHandle"},
		"no earlier copy": {"a.png", ""},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			inner := &physicalFileStub{savedPath: "local://7/exports/new.png"}
			catalog := &catalogStub{reused: c.reused}
			svc := NewResourceCatalogFileService(inner, catalog)

			ref, err := svc.SaveBytes(context.Background(), []byte("bytes"), 7, c.name, label == "temporary")
			require.NoError(t, err)
			require.Equal(t, "resource://AbCdEfGhIjKlMnOpQrStUv", ref, "bytes register as usual")
			require.NotNil(t, catalog.resource)
			require.Equal(t, "local://7/exports/new.png", catalog.resource.PhysicalPath, "bytes are written and registered")
		})
	}
}

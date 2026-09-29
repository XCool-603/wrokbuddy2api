package dshmgr

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestUnzipAndResolve(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "dshmgr_test_*")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 创建一个模拟的 zip 压缩包，模拟 Node.js 压缩包解压后的顶层子目录
	zipBuffer := new(bytes.Buffer)
	zipWriter := zip.NewWriter(zipBuffer)

	// 添加 node-v20.18.0-win-x64/node.exe
	nodeFile, err := zipWriter.Create("node-v20.18.0-win-x64/node.exe")
	if err != nil {
		t.Fatalf("创建 zip 条目失败: %v", err)
	}
	_, _ = nodeFile.Write([]byte("fake node binary"))

	// 添加 node-v20.18.0-win-x64/npx.cmd
	npxFile, err := zipWriter.Create("node-v20.18.0-win-x64/npx.cmd")
	if err != nil {
		t.Fatalf("创建 zip 条目失败: %v", err)
	}
	_, _ = npxFile.Write([]byte("fake npx command"))

	if err := zipWriter.Close(); err != nil {
		t.Fatalf("关闭 zipWriter 失败: %v", err)
	}

	zipPath := filepath.Join(tempDir, "mock_node.zip")
	if err := os.WriteFile(zipPath, zipBuffer.Bytes(), 0644); err != nil {
		t.Fatalf("写入 zip 文件失败: %v", err)
	}

	extractDir := filepath.Join(tempDir, "tmp_extract")
	if err := unzip(zipPath, extractDir); err != nil {
		t.Fatalf("unzip 失败: %v", err)
	}

	// 验证 findExecutableInDir
	foundNode := findExecutableInDir(extractDir, "node.exe")
	if foundNode == "" {
		t.Fatalf("findExecutableInDir 未找到 node.exe")
	}

	// 测试部署与 resolveNodeNpx
	mgr := New(tempDir)
	targetDir := mgr.localBinDir()
	_ = os.MkdirAll(filepath.Dir(targetDir), 0755)

	parentDir := filepath.Dir(foundNode)
	var sourceDir string
	if filepath.Base(parentDir) == "bin" {
		sourceDir = filepath.Dir(parentDir)
	} else {
		sourceDir = parentDir
	}

	if err := copyDir(sourceDir, targetDir); err != nil {
		t.Fatalf("copyDir 失败: %v", err)
	}

	nodePath, npxPath, ok := mgr.resolveNodeNpx()
	if !ok {
		t.Fatalf("resolveNodeNpx 失败: node=%s, npx=%s", nodePath, npxPath)
	}
	if filepath.Base(nodePath) != "node.exe" {
		t.Errorf("期望 node.exe, 实际: %s", nodePath)
	}
	if filepath.Base(npxPath) != "npx.cmd" && filepath.Base(npxPath) != "npx" {
		t.Errorf("期望 npx.cmd 或 npx, 实际: %s", npxPath)
	}
}

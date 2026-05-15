package step

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bitrise-io/go-utils/v2/command"
	"github.com/bitrise-io/go-utils/v2/env"
	"github.com/bitrise-io/go-utils/v2/log"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/api"
	"github.com/bitrise-steplib/bitrise-step-pull-intermediate-files/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Run_DownloadsPlainFile(t *testing.T) {
	const appSlug = "test-app"
	fileContent := []byte("hello, world")

	srv := buildMockServer(t, appSlug, map[string][]mockArtifact{
		"build-1": {{slug: "artifact-1", title: "file.txt", envKey: "TEXT_FILE_TXT", fileContent: fileContent}},
	})
	cfg := stagedConfig(appSlug, srv.URL, []string{".*"}, model.FinishedStages{
		{Name: "stage-1", Workflows: []model.Workflow{{ExternalId: "build-1", Name: "workflow-a"}}},
	})

	result, err := setupDownloader(t).Run(cfg)

	require.NoError(t, err)
	require.Len(t, result.IntermediateFiles, 1)
	got, err := os.ReadFile(result.IntermediateFiles["TEXT_FILE_TXT"])
	require.NoError(t, err)
	assert.Equal(t, fileContent, got)
}

func Test_Run_DownloadsZipArchive(t *testing.T) {
	const appSlug = "test-app"
	zipContent := createZipArchive(t, map[string]string{
		"file1.txt": "content-one",
		"file2.txt": "content-two",
	})

	srv := buildMockServer(t, appSlug, map[string][]mockArtifact{
		"build-1": {{slug: "artifact-1", title: "archive.zip", envKey: "ARCHIVE_ZIP", isDir: true, fileContent: zipContent}},
	})
	cfg := stagedConfig(appSlug, srv.URL, []string{".*"}, model.FinishedStages{
		{Name: "stage-1", Workflows: []model.Workflow{{ExternalId: "build-1", Name: "workflow-a"}}},
	})

	result, err := setupDownloader(t).Run(cfg)

	require.NoError(t, err)
	require.Len(t, result.IntermediateFiles, 1)
	archiveDir := result.IntermediateFiles["ARCHIVE_ZIP"]
	require.DirExists(t, archiveDir)
	got, err := os.ReadFile(filepath.Join(archiveDir, "file1.txt"))
	require.NoError(t, err)
	assert.Equal(t, "content-one", string(got))
	assert.FileExists(t, filepath.Join(archiveDir, "file2.txt"))
}

func Test_Run_DownloadsTarArchive(t *testing.T) {
	const appSlug = "test-app"
	tarContent := createTarArchive(t, map[string]string{
		"file1.txt": "tar-content-one",
		"file2.txt": "tar-content-two",
	})

	srv := buildMockServer(t, appSlug, map[string][]mockArtifact{
		"build-1": {{slug: "artifact-1", title: "archive.tar", envKey: "ARCHIVE_TAR", isDir: true, fileContent: tarContent}},
	})
	cfg := stagedConfig(appSlug, srv.URL, []string{".*"}, model.FinishedStages{
		{Name: "stage-1", Workflows: []model.Workflow{{ExternalId: "build-1", Name: "workflow-a"}}},
	})

	result, err := setupDownloader(t).Run(cfg)

	require.NoError(t, err)
	require.Len(t, result.IntermediateFiles, 1)
	archiveDir := result.IntermediateFiles["ARCHIVE_TAR"]
	require.DirExists(t, archiveDir)
	assert.FileExists(t, filepath.Join(archiveDir, "file1.txt"))
	assert.FileExists(t, filepath.Join(archiveDir, "file2.txt"))
}

func Test_Run_DownloadsFromMultipleBuilds(t *testing.T) {
	const appSlug = "test-app"

	srv := buildMockServer(t, appSlug, map[string][]mockArtifact{
		"build-1": {{slug: "artifact-1", title: "text.txt", envKey: "TEXT_FILE_TXT", fileContent: []byte("text")}},
		"build-2": {{slug: "artifact-2", title: "data.json", envKey: "DATA_JSON", fileContent: []byte("{}")}},
	})
	cfg := stagedConfig(appSlug, srv.URL, []string{".*"}, model.FinishedStages{
		{Name: "stage-1", Workflows: []model.Workflow{{ExternalId: "build-1", Name: "workflow-a"}}},
		{Name: "stage-2", Workflows: []model.Workflow{{ExternalId: "build-2", Name: "workflow-b"}}},
	})

	result, err := setupDownloader(t).Run(cfg)

	require.NoError(t, err)
	assert.Len(t, result.IntermediateFiles, 2)
	assert.NotEmpty(t, result.IntermediateFiles["TEXT_FILE_TXT"])
	assert.NotEmpty(t, result.IntermediateFiles["DATA_JSON"])
}

func Test_Run_FiltersByStagePattern(t *testing.T) {
	const appSlug = "test-app"

	srv := buildMockServer(t, appSlug, map[string][]mockArtifact{
		"build-1": {{slug: "artifact-1", title: "text.txt", envKey: "TEXT_FILE_TXT", fileContent: []byte("text")}},
		"build-2": {{slug: "artifact-2", title: "data.csv", envKey: "DATA_CSV", fileContent: []byte("a,b")}},
	})
	cfg := stagedConfig(appSlug, srv.URL, []string{`stage-1\..*`}, model.FinishedStages{
		{Name: "stage-1", Workflows: []model.Workflow{{ExternalId: "build-1", Name: "workflow-a"}}},
		{Name: "stage-2", Workflows: []model.Workflow{{ExternalId: "build-2", Name: "workflow-b"}}},
	})

	result, err := setupDownloader(t).Run(cfg)

	require.NoError(t, err)
	assert.Len(t, result.IntermediateFiles, 1)
	assert.NotEmpty(t, result.IntermediateFiles["TEXT_FILE_TXT"])
	assert.Empty(t, result.IntermediateFiles["DATA_CSV"])
}

// --- Helpers ---

type mockArtifact struct {
	slug        string
	title       string
	envKey      string
	isDir       bool
	fileContent []byte
}

func stagedConfig(appSlug, apiBaseURL string, sources []string, stages model.FinishedStages) Config {
	return Config{
		ArtifactSources:       sources,
		AppSlug:               appSlug,
		FinishedStages:        stages,
		BitriseAPIBaseURL:     apiBaseURL,
		BitriseAPIAccessToken: "test-token",
	}
}

func setupDownloader(t *testing.T) IntermediateFileDownloader {
	t.Helper()
	envRepo := env.NewRepository()
	return IntermediateFileDownloader{
		cmdFactory:    command.NewFactory(envRepo),
		envRepository: envRepo,
		logger:        log.NewLogger(),
	}
}

// buildMockServer creates a test HTTP server that handles both Bitrise API calls
// (list + show artifact) and file downloads for the given artifacts.
// buildArtifacts maps build slugs to their artifacts.
func buildMockServer(t *testing.T, appSlug string, buildArtifacts map[string][]mockArtifact) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	mux := http.NewServeMux()

	for buildSlug, artifacts := range buildArtifacts {
		bs := buildSlug
		as := artifacts

		mux.HandleFunc(fmt.Sprintf("/v0.2/apps/%s/builds/%s/artifacts", appSlug, bs), func(w http.ResponseWriter, r *http.Request) {
			var items []api.ArtifactListElementResponseModel
			for _, a := range as {
				items = append(items, api.ArtifactListElementResponseModel{Slug: a.slug, Title: a.title})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.ListBuildArtifactsResponse{Data: items})
		})

		for _, artifact := range artifacts {
			a := artifact

			mux.HandleFunc(fmt.Sprintf("/v0.2/apps/%s/builds/%s/artifacts/%s", appSlug, bs, a.slug), func(w http.ResponseWriter, r *http.Request) {
				item := api.ArtifactResponseItemModel{
					Title:       a.title,
					Slug:        a.slug,
					DownloadURL: srv.URL + "/download/" + a.slug,
					IntermediateFileInfo: api.IntermediateFileInfo{
						EnvKey: a.envKey,
						IsDir:  a.isDir,
					},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(api.ShowBuildArtifactResponse{Data: item})
			})

			mux.HandleFunc("/download/"+a.slug, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(a.fileContent)
			})
		}
	}

	srv = httptest.NewUnstartedServer(mux)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func createZipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func createTarArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, content := range files {
		err := w.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))})
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

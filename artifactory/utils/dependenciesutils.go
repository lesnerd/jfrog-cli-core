package utils

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/jfrog/jfrog-cli-core/utils/config"
	"github.com/jfrog/jfrog-cli-core/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/http/httpclient"
	"github.com/jfrog/jfrog-client-go/http/jfroghttpclient"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

const (
	// This env var should be used for downloading the extractor jars through an Artifactory remote
	// repository, instead of downloading directly from ojo. The remote repository should be
	// configured to proxy ojo.

	// Deprecated. This env var should store a server ID configured by JFrog CLI.
	JCenterRemoteServerEnv = "JFROG_CLI_JCENTER_REMOTE_SERVER"
	// Deprecated. If the JCenterRemoteServerEnv env var is used, a maven remote repository named jcenter is assumed.
	// This env var can be used to use a different remote repository name.
	JCenterRemoteRepoEnv = "JFROG_CLI_JCENTER_REMOTE_REPO"

	// This env var should store a server ID and a remote repository in form of '<ServerID>/<RemoteRepo>'
	ExtractorsRemoteEnv = "JFROG_CLI_EXTRACTORS_REMOTE"
)

// Download the relevant build-info-extractor jar, if it does not already exist locally.
// By default, the jar is downloaded directly from jcenter.
// If the JCenterRemoteServerEnv environment variable is configured, the jar will be
// downloaded from a remote Artifactory repository which proxies jcenter.
//
// downloadPath: The Bintray or Artifactory download path.
// filename: The local file name.
// targetPath: The local download path (without the file name).
func DownloadExtractorIfNeeded(downloadPath, targetPath string) error {
	// If the file exists locally, we're done.
	exists, err := fileutils.IsFileExists(targetPath, false)
	if exists || err != nil {
		return err
	}
	log.Info("The build-info-extractor jar is not cached locally. Downloading it now...\n You can set the repository from which this jar is downloaded. Read more about it at https://www.jfrog.com/confluence/display/CLI/CLI+for+JFrog+Artifactory#CLIforJFrogArtifactory-DownloadingtheMavenandGradleExtractorJARs")
	artDetails, remotePath, err := GetExtractorsRemoteDetails(downloadPath)
	if err != nil {
		return err
	}

	return downloadExtractor(artDetails, remotePath, targetPath)
}

func GetExtractorsRemoteDetails(downloadPath string) (*config.ServerDetails, string, error) {
	// Download through a remote repository in Artifactory, if configured to do so.
	jCenterRemoteServer := os.Getenv(JCenterRemoteServerEnv)
	if jCenterRemoteServer != "" {
		return getJcenterRemoteDetails(jCenterRemoteServer, downloadPath)
	}
	extractorsRemote := os.Getenv(ExtractorsRemoteEnv)
	if extractorsRemote != "" {
		return getExtractorsRemoteDetails(extractorsRemote, downloadPath)
	}

	log.Debug("'" + ExtractorsRemoteEnv + "' environment variable is not configured. Downloading directly from oss.jfrog.org.")
	// If not configured to download through a remote repository in Artifactory, download from ojo.
	return &config.ServerDetails{ArtifactoryUrl: "https://oss.jfrog.org/artifactory/"}, path.Join("oss-release-local", downloadPath), nil
}

// Deprecated. Return the version of the build-info extractor to download.
// If 'JFROG_CLI_JCENTER_REMOTE_SERVER' is used, choose the latest published JCenter version.
func GetExtractorVersion(ojoVersion, jCenterVersion string) string {
	if os.Getenv(JCenterRemoteServerEnv) != "" {
		return jCenterVersion
	}
	return ojoVersion
}

// Deprecated. Get Artifactory server details and a repository proxying JCenter/oss.jfrog.org according to 'JFROG_CLI_JCENTER_REMOTE_SERVER' and 'JFROG_CLI_JCENTER_REMOTE_REPO' env vars.
func getJcenterRemoteDetails(serverId, downloadPath string) (*config.ServerDetails, string, error) {
	log.Warn(`It looks like the 'JFROG_CLI_JCENTER_REMOTE_SERVER' or 'JFROG_CLI_JCENTER_REMOTE_REPO' environment variables are set.
	These environment variables were used by the JFrog CLI to download the build-info extractors JARs for Maven and Gradle builds. 
	These environment variables are now deprecated. 
	For more information, please refer to the documentation at https://www.jfrog.com/confluence/display/CLI/CLI+for+JFrog+Artifactory#CLIforJFrogArtifactory-DownloadingtheMavenandGradleExtractorJARs.`)
	serverDetails, err := config.GetSpecificConfig(serverId, false, true)
	repoName := os.Getenv(JCenterRemoteRepoEnv)
	if repoName == "" {
		repoName = "jcenter"
	}
	return serverDetails, path.Join(repoName, downloadPath), err
}

// Get Artifactory server details and a repository proxying oss.jfrog.org according to JFROG_CLI_EXTRACTORS_REMOTE env var.
func getExtractorsRemoteDetails(extractorsRemote, downloadPath string) (*config.ServerDetails, string, error) {
	lastSlashIndex := strings.LastIndex(extractorsRemote, "/")
	if lastSlashIndex == -1 {
		return nil, "", errorutils.CheckError(errors.New(fmt.Sprintf("'%s' environment variable is '%s' but should be '<server ID>/<repo name>'.", ExtractorsRemoteEnv, extractorsRemote)))
	}

	serverDetails, err := config.GetSpecificConfig(extractorsRemote[:lastSlashIndex], false, true)
	repoName := extractorsRemote[lastSlashIndex+1:]
	return serverDetails, path.Join(repoName, downloadPath), err
}

func downloadExtractor(artDetails *config.ServerDetails, downloadPath, targetPath string) error {
	downloadUrl := fmt.Sprintf("%s%s", artDetails.ArtifactoryUrl, downloadPath)
	log.Info("Downloading build-info-extractor from", downloadUrl)
	filename, localDir := fileutils.GetFileAndDirFromPath(targetPath)

	downloadFileDetails := &httpclient.DownloadFileDetails{
		FileName:      filename,
		DownloadPath:  downloadUrl,
		LocalPath:     localDir,
		LocalFileName: filename,
	}

	auth, err := artDetails.CreateArtAuthConfig()
	if err != nil {
		return err
	}
	certsPath, err := coreutils.GetJfrogCertsDir()
	if err != nil {
		return err
	}

	client, err := jfroghttpclient.JfrogClientBuilder().
		SetCertificatesPath(certsPath).
		SetInsecureTls(artDetails.InsecureTls).
		SetClientCertPath(auth.GetClientCertPath()).
		SetClientCertKeyPath(auth.GetClientCertKeyPath()).
		AppendPreRequestInterceptor(auth.RunPreRequestFunctions).
		Build()
	if err != nil {
		return err
	}

	httpClientDetails := auth.CreateHttpClientDetails()
	resp, err := client.DownloadFile(downloadFileDetails, "", &httpClientDetails, 3, false)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = errorutils.CheckError(errors.New(resp.Status + " received when attempting to download " + downloadUrl))
	}

	return err
}

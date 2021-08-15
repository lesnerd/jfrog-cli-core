package npm

import (
	"bufio"
	"errors"
	"fmt"
	commandUtils "github.com/jfrog/jfrog-cli-core/artifactory/commands/utils"
	npmutils "github.com/jfrog/jfrog-cli-core/utils/npm"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buger/jsonparser"
	gofrogcmd "github.com/jfrog/gofrog/io"
	"github.com/jfrog/gofrog/parallel"
	"github.com/jfrog/jfrog-cli-core/artifactory/utils"
	"github.com/jfrog/jfrog-cli-core/artifactory/utils/npm"
	"github.com/jfrog/jfrog-cli-core/utils/config"
	"github.com/jfrog/jfrog-client-go/artifactory"
	"github.com/jfrog/jfrog-client-go/artifactory/buildinfo"
	"github.com/jfrog/jfrog-client-go/auth"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/jfrog/jfrog-client-go/utils/version"
)

const npmrcFileName = ".npmrc"
const npmrcBackupFileName = "jfrog.npmrc.backup"
const minSupportedNpmVersion = "5.4.0"

type NpmCommandArgs struct {
	command          string
	threads          int
	jsonOutput       bool
	executablePath   string
	restoreNpmrcFunc func() error
	workingDirectory string
	registry         string
	npmAuth          string
	collectBuildInfo bool
	dependencies     map[string]*dependency
	typeRestriction  typeRestriction
	authArtDetails   auth.ServiceDetails
	packageInfo      *npmutils.PackageInfo
	npmVersion       *version.Version
	NpmCommand
}

type typeRestriction int

const (
	defaultRestriction typeRestriction = iota
	all
	devOnly
	prodOnly
)

type NpmInstallOrCiCommand struct {
	configFilePath      string
	internalCommandName string
	*NpmCommandArgs
}

func NewNpmInstallCommand() *NpmInstallOrCiCommand {
	return &NpmInstallOrCiCommand{NpmCommandArgs: NewNpmCommandArgs("install"), internalCommandName: "rt_npm_install"}
}

func NewNpmCiCommand() *NpmInstallOrCiCommand {
	return &NpmInstallOrCiCommand{NpmCommandArgs: NewNpmCommandArgs("ci"), internalCommandName: "rt_npm_ci"}
}

func (nic *NpmInstallOrCiCommand) CommandName() string {
	return nic.internalCommandName
}

func (nic *NpmInstallOrCiCommand) SetConfigFilePath(configFilePath string) *NpmInstallOrCiCommand {
	nic.configFilePath = configFilePath
	return nic
}

func (nic *NpmInstallOrCiCommand) SetArgs(args []string) *NpmInstallOrCiCommand {
	nic.NpmCommandArgs.npmArgs = args
	return nic
}

func (nic *NpmInstallOrCiCommand) SetRepoConfig(conf *utils.RepositoryConfig) *NpmInstallOrCiCommand {
	serverDetails, _ := conf.ServerDetails()
	nic.NpmCommandArgs.SetRepo(conf.TargetRepo()).SetServerDetails(serverDetails)
	return nic
}

func (nic *NpmInstallOrCiCommand) Run() error {
	log.Info(fmt.Sprintf("Running npm %s.", nic.command))
	// Read config file.
	log.Debug("Preparing to read the config file", nic.configFilePath)
	vConfig, err := utils.ReadConfigFile(nic.configFilePath, utils.YAML)
	if err != nil {
		return err
	}
	// Extract resolution params.
	resolverParams, err := utils.GetRepoConfigByPrefix(nic.configFilePath, utils.ProjectConfigResolverPrefix, vConfig)
	if err != nil {
		return err
	}
	threads, _, filteredNpmArgs, buildConfiguration, err := commandUtils.ExtractNpmOptionsFromArgs(nic.npmArgs)
	if err != nil {
		return err
	}
	nic.SetRepoConfig(resolverParams).SetArgs(filteredNpmArgs).SetThreads(threads).SetBuildConfiguration(buildConfiguration)
	return nic.run()
}

func (nca *NpmCommandArgs) SetThreads(threads int) *NpmCommandArgs {
	nca.threads = threads
	return nca
}

func NewNpmCommandArgs(npmCommand string) *NpmCommandArgs {
	return &NpmCommandArgs{command: npmCommand}
}

func (nca *NpmCommandArgs) ServerDetails() (*config.ServerDetails, error) {
	return nca.serverDetails, nil
}

func (nca *NpmCommandArgs) run() error {
	if err := nca.preparePrerequisites(nca.repo); err != nil {
		return err
	}

	if err := nca.createTempNpmrc(); err != nil {
		return nca.restoreNpmrcAndError(err)
	}

	if err := nca.runInstallOrCi(); err != nil {
		return nca.restoreNpmrcAndError(err)
	}

	if err := nca.restoreNpmrcFunc(); err != nil {
		return err
	}

	if !nca.collectBuildInfo {
		log.Info(fmt.Sprintf("npm %s finished successfully.", nca.command))
		return nil
	}

	if err := nca.setDependenciesList(); err != nil {
		return err
	}

	if err := nca.collectDependenciesChecksums(); err != nil {
		return err
	}

	if err := nca.saveDependenciesData(); err != nil {
		return err
	}

	log.Info(fmt.Sprintf("npm %s finished successfully.", nca.command))
	return nil
}

func (nca *NpmCommandArgs) preparePrerequisites(repo string) error {
	log.Debug("Preparing prerequisites.")
	var err error
	if err = nca.setNpmExecutable(); err != nil {
		return err
	}

	if err = nca.validateNpmVersion(); err != nil {
		return err
	}

	if err := nca.setJsonOutput(); err != nil {
		return err
	}

	nca.workingDirectory, err = commandUtils.GetWorkingDirectory()
	if err != nil {
		return err
	}
	log.Debug("Working directory set to:", nca.workingDirectory)

	if err = nca.setArtifactoryAuth(); err != nil {
		return err
	}

	nca.npmAuth, nca.registry, err = commandUtils.GetArtifactoryNpmRepoDetails(repo, &nca.authArtDetails)
	if err != nil {
		return err
	}

	nca.collectBuildInfo, nca.packageInfo, err = commandUtils.PrepareBuildInfo(nca.workingDirectory, nca.buildConfiguration, nca.npmVersion)
	if err != nil {
		return err
	}

	nca.restoreNpmrcFunc, err = commandUtils.BackupFile(filepath.Join(nca.workingDirectory, npmrcFileName), filepath.Join(nca.workingDirectory, npmrcBackupFileName))
	return err
}

func (nca *NpmCommandArgs) setJsonOutput() error {
	jsonOutput, err := npm.ConfigGet(nca.npmArgs, "json", nca.executablePath)
	if err != nil {
		return err
	}

	// In case of --json=<not boolean>, the value of json is set to 'true', but the result from the command is not 'true'
	nca.jsonOutput = jsonOutput != "false"
	return nil
}

func createRestoreErrorPrefix(workingDirectory string) string {
	return fmt.Sprintf("Error occurred while restoring project .npmrc file. "+
		"Delete '%s' and move '%s' (if exists) to '%s' in order to restore the project. Failure cause: \n",
		filepath.Join(workingDirectory, npmrcFileName),
		filepath.Join(workingDirectory, npmrcBackupFileName),
		filepath.Join(workingDirectory, npmrcFileName))
}

// In order to make sure the install/ci downloads the artifacts from Artifactory we create a .npmrc file in the project dir.
// If such a file exists we back it up as npmrcBackupFileName.
func (nca *NpmCommandArgs) createTempNpmrc() error {
	log.Debug("Creating project .npmrc file.")
	data, err := npm.GetConfigList(nca.npmArgs, nca.executablePath)
	configData, err := nca.prepareConfigData(data)
	if err != nil {
		return errorutils.CheckError(err)
	}

	if err = removeNpmrcIfExists(nca.workingDirectory); err != nil {
		return err
	}

	return errorutils.CheckError(ioutil.WriteFile(filepath.Join(nca.workingDirectory, npmrcFileName), configData, 0600))
}

func (nca *NpmCommandArgs) runInstallOrCi() error {
	log.Debug(fmt.Sprintf("Running npm %s command.", nca.command))
	filteredArgs := filterFlags(nca.npmArgs)
	npmCmdConfig := &npm.NpmConfig{
		Npm:          nca.executablePath,
		Command:      append([]string{nca.command}, filteredArgs...),
		CommandFlags: nil,
		StrWriter:    nil,
		ErrWriter:    nil,
	}

	if nca.collectBuildInfo && len(filteredArgs) > 0 {
		log.Warn("Build info dependencies collection with npm arguments is not supported. Build info creation will be skipped.")
		nca.collectBuildInfo = false
	}

	return errorutils.CheckError(gofrogcmd.RunCmd(npmCmdConfig))
}

func (nca *NpmCommandArgs) setDependenciesList() (err error) {
	nca.dependencies = make(map[string]*dependency)
	// nca.typeRestriction default is 'all'
	if nca.typeRestriction != prodOnly {
		if err = nca.prepareDependencies("dev"); err != nil {
			return
		}
	}
	if nca.typeRestriction != devOnly {
		err = nca.prepareDependencies("prod")
	}
	return
}

func (nca *NpmCommandArgs) collectDependenciesChecksums() error {
	log.Info("Collecting dependencies information... For the first run of the build, this may take a few minutes. Subsequent runs should be faster.")
	servicesManager, err := utils.CreateServiceManager(nca.serverDetails, -1, false)
	if err != nil {
		return err
	}

	previousBuildDependencies, err := commandUtils.GetDependenciesFromLatestBuild(servicesManager, nca.buildConfiguration.BuildName)
	if err != nil {
		return err
	}
	producerConsumer := parallel.NewBounedRunner(nca.threads, false)
	errorsQueue := clientutils.NewErrorsQueue(1)
	handlerFunc := nca.createGetDependencyInfoFunc(servicesManager, previousBuildDependencies)
	go func() {
		defer producerConsumer.Done()
		for i := range nca.dependencies {
			producerConsumer.AddTaskWithError(handlerFunc(i), errorsQueue.AddError)
		}
	}()
	producerConsumer.Run()
	return errorsQueue.GetError()
}

func (nca *NpmCommandArgs) saveDependenciesData() error {
	log.Debug("Saving data.")
	if nca.buildConfiguration.Module == "" {
		nca.buildConfiguration.Module = nca.packageInfo.BuildInfoModuleId()
	}

	dependencies, missingDependencies := nca.transformDependencies()
	if err := commandUtils.SaveDependenciesData(dependencies, nca.buildConfiguration); err != nil {
		return err
	}

	commandUtils.PrintMissingDependencies(missingDependencies)
	return nil
}

func (nca *NpmCommandArgs) validateNpmVersion() error {
	npmVersion, err := npmutils.Version(nca.executablePath)
	if err != nil {
		return err
	}
	if npmVersion.Compare(minSupportedNpmVersion) > 0 {
		return errorutils.CheckError(errors.New(fmt.Sprintf(
			"JFrog CLI npm %s command requires npm client version "+minSupportedNpmVersion+" or higher", nca.command)))
	}
	nca.npmVersion = npmVersion
	return nil
}

// This func transforms "npm config list" result to key=val list of values that can be set to .npmrc file.
// it filters any nil values key, changes registry and scope registries to Artifactory url and adds Artifactory authentication to the list
func (nca *NpmCommandArgs) prepareConfigData(data []byte) ([]byte, error) {
	var filteredConf []string
	configString := string(data)
	scanner := bufio.NewScanner(strings.NewReader(configString))

	for scanner.Scan() {
		currOption := scanner.Text()
		if currOption != "" {
			splitOption := strings.SplitN(currOption, "=", 2)
			key := strings.TrimSpace(splitOption[0])
			if len(splitOption) == 2 && isValidKey(key) {
				value := strings.TrimSpace(splitOption[1])
				if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
					filteredConf = addArrayConfigs(filteredConf, key, value)
				} else {
					filteredConf = append(filteredConf, currOption, "\n")
				}
				nca.setTypeRestriction(key, value)
			} else if strings.HasPrefix(splitOption[0], "@") {
				// Override scoped registries (@scope = xyz)
				filteredConf = append(filteredConf, splitOption[0], " = ", nca.registry, "\n")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errorutils.CheckError(err)
	}

	filteredConf = append(filteredConf, "json = ", strconv.FormatBool(nca.jsonOutput), "\n")
	filteredConf = append(filteredConf, "registry = ", nca.registry, "\n")
	filteredConf = append(filteredConf, nca.npmAuth)
	return []byte(strings.Join(filteredConf, "")), nil
}

// Gets a config with value which is an array, and adds it to the conf list
func addArrayConfigs(conf []string, key, arrayValue string) []string {
	if arrayValue == "[]" {
		return conf
	}

	values := strings.TrimPrefix(strings.TrimSuffix(arrayValue, "]"), "[")
	valuesSlice := strings.Split(values, ",")
	for _, val := range valuesSlice {
		confToAdd := fmt.Sprintf("%s[] = %s", key, val)
		conf = append(conf, confToAdd, "\n")
	}

	return conf
}

func (nca *NpmCommandArgs) setTypeRestriction(key string, value string) {
	// From npm 7, type restriction is determined by 'omit' and 'include' (both appear in 'npm config ls').
	// Other options (like 'dev', 'production' and 'only') are deprecated, but if they're used anyway - 'omit' and 'include' are automatically calculated.
	// So 'omit' is always preferred, if it exists.
	if key == "omit" {
		if strings.Contains(value, "dev") {
			nca.typeRestriction = prodOnly
		} else {
			nca.typeRestriction = all
		}
	} else if nca.typeRestriction == defaultRestriction { // Until npm 6, configurations in 'npm config ls' are sorted by priority in descending order, so typeRestriction should be set only if it was not set before
		if key == "only" {
			if strings.Contains(value, "prod") {
				nca.typeRestriction = prodOnly
			} else if strings.Contains(value, "dev") {
				nca.typeRestriction = devOnly
			}
		} else if key == "production" && strings.Contains(value, "true") {
			nca.typeRestriction = prodOnly
		}
	}
}

// Run npm list and parse the returned JSON.
// typeRestriction must be one of: 'dev' or 'prod'!
func (nca *NpmCommandArgs) prepareDependencies(typeRestriction string) error {
	// Run npm list
	// Although this command can get --development as a flag (according to npm docs), it's not working on npm 6.
	// Although this command can get --only=development as a flag (according to npm docs), it's not working on npm 7.
	data, errData, err := npm.RunList(strings.Join(append(nca.npmArgs, "--all", "--"+typeRestriction), " "), nca.executablePath)
	if err != nil {
		log.Warn("npm list command failed with error:", err.Error())
	}
	if len(errData) > 0 {
		log.Warn("Some errors occurred while collecting dependencies info:\n" + string(errData))
	}

	// Parse the dependencies json object
	return jsonparser.ObjectEach(data, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) (err error) {
		if string(key) == "dependencies" {
			err = nca.parseDependencies(value, typeRestriction, []string{nca.packageInfo.BuildInfoModuleId()})
		}
		return err
	})
}

// Parses npm dependencies recursively and adds the collected dependencies to nca.dependencies
func (nca *NpmCommandArgs) parseDependencies(data []byte, scope string, pathToRoot []string) error {
	return jsonparser.ObjectEach(data, func(key []byte, value []byte, dataType jsonparser.ValueType, offset int) error {
		depName := string(key)
		ver, _, _, err := jsonparser.Get(data, depName, "version")
		depVersion := string(ver)
		depKey := depName + ":" + depVersion
		if err != nil && err != jsonparser.KeyPathNotFoundError {
			return errorutils.CheckError(err)
		} else if err == jsonparser.KeyPathNotFoundError {
			log.Debug(fmt.Sprintf("%s dependency will not be included in the build-info, because the 'npm ls' command did not return its version.\nThe reason why the version wasn't returned may be because the package is a 'peerdependency', which was not manually installed.\n'npm install' does not download 'peerdependencies' automatically. It is therefore okay to skip this dependency.", depName))
		} else {
			nca.appendDependency(depKey, depName, depVersion, scope, pathToRoot)
		}
		transitive, _, _, err := jsonparser.Get(data, depName, "dependencies")
		if err != nil && err.Error() != "Key path not found" {
			return errorutils.CheckError(err)
		}
		if len(transitive) > 0 {
			if err := nca.parseDependencies(transitive, scope, append([]string{depKey}, pathToRoot...)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (nca *NpmCommandArgs) appendDependency(depKey, depName, depVersion, scope string, pathToRoot []string) {
	if nca.dependencies[depKey] == nil {
		nca.dependencies[depKey] = &dependency{name: depName, version: depVersion, scopes: []string{scope}}
	} else if !scopeAlreadyExists(scope, nca.dependencies[depKey].scopes) {
		nca.dependencies[depKey].scopes = append(nca.dependencies[depKey].scopes, scope)
	}
	nca.dependencies[depKey].pathToRoot = append(nca.dependencies[depKey].pathToRoot, pathToRoot)
}

// Creates a function that fetches dependency data.
// If a dependency was included in the previous build, take the checksums information from it.
// Otherwise, fetch the checksum from Artifactory.
// Can be applied from a producer-consumer mechanism.
func (nca *NpmCommandArgs) createGetDependencyInfoFunc(servicesManager artifactory.ArtifactoryServicesManager,
	previousBuildDependencies map[string]*buildinfo.Dependency) getDependencyInfoFunc {
	return func(dependencyIndex string) parallel.TaskFunc {
		return func(threadId int) error {
			name := nca.dependencies[dependencyIndex].name
			ver := nca.dependencies[dependencyIndex].version

			// Get dependency info.
			checksum, fileType, err := commandUtils.GetDependencyInfo(name, ver, previousBuildDependencies, servicesManager, threadId)
			if err != nil || checksum == nil {
				return err
			}

			// Update dependency.
			nca.dependencies[dependencyIndex].fileType = fileType
			nca.dependencies[dependencyIndex].checksum = checksum
			return nil
		}
	}
}

// Transforms the list of dependencies to buildinfo.Dependencies list and creates a list of dependencies that are missing in Artifactory.
func (nca *NpmCommandArgs) transformDependencies() (dependencies []buildinfo.Dependency, missingDependencies []buildinfo.Dependency) {
	for _, dependency := range nca.dependencies {
		biDependency := buildinfo.Dependency{Id: dependency.name + ":" + dependency.version, Type: dependency.fileType,
			Scopes: dependency.scopes, Checksum: dependency.checksum, RequestedBy: dependency.pathToRoot}
		if dependency.checksum != nil {
			dependencies = append(dependencies,
				biDependency)
		} else {
			missingDependencies = append(missingDependencies, biDependency)
		}
	}
	return
}

func (nca *NpmCommandArgs) restoreNpmrcAndError(err error) error {
	if restoreErr := nca.restoreNpmrcFunc(); restoreErr != nil {
		return errorutils.CheckError(errors.New(fmt.Sprintf("Two errors occurred:\n %s\n %s", restoreErr.Error(), err.Error())))
	}
	return err
}

func (nca *NpmCommandArgs) setArtifactoryAuth() error {
	authArtDetails, err := nca.serverDetails.CreateArtAuthConfig()
	if err != nil {
		return err
	}
	if authArtDetails.GetSshAuthHeaders() != nil {
		return errorutils.CheckError(errors.New("SSH authentication is not supported in this command"))
	}
	nca.authArtDetails = authArtDetails
	return nil
}

func removeNpmrcIfExists(workingDirectory string) error {
	if _, err := os.Stat(filepath.Join(workingDirectory, npmrcFileName)); err != nil {
		if os.IsNotExist(err) { // The file dose not exist, nothing to do.
			return nil
		}
		return errorutils.CheckError(err)
	}

	log.Debug("Removing Existing .npmrc file")
	return errorutils.CheckError(os.Remove(filepath.Join(workingDirectory, npmrcFileName)))
}

func (nca *NpmCommandArgs) setNpmExecutable() error {
	npmExecPath, err := exec.LookPath("npm")
	if err != nil {
		return errorutils.CheckError(err)
	}

	if npmExecPath == "" {
		return errorutils.CheckError(errors.New("could not find 'npm' executable"))
	}
	nca.executablePath = npmExecPath
	log.Debug("Found npm executable at:", nca.executablePath)
	return nil
}

func scopeAlreadyExists(scope string, existingScopes []string) bool {
	for _, existingScope := range existingScopes {
		if existingScope == scope {
			return true
		}
	}
	return false
}

// To avoid writing configurations that are used by us
func isValidKey(key string) bool {
	return !strings.HasPrefix(key, "//") &&
		!strings.HasPrefix(key, ";") && // Comments
		!strings.HasPrefix(key, "@") && // Scoped configurations
		key != "registry" &&
		key != "metrics-registry" &&
		key != "json" // Handled separately because 'npm c ls' should run with json=false
}

func filterFlags(splitArgs []string) []string {
	var filteredArgs []string
	for _, arg := range splitArgs {
		if !strings.HasPrefix(arg, "-") {
			filteredArgs = append(filteredArgs, arg)
		}
	}
	return filteredArgs
}

type getDependencyInfoFunc func(string) parallel.TaskFunc

type dependency struct {
	name       string
	version    string
	scopes     []string
	fileType   string
	checksum   *buildinfo.Checksum
	pathToRoot [][]string
}

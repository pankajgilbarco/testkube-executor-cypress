package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	junit "github.com/joshdk/go-junit"
	"github.com/kelseyhightower/envconfig"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/content"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/executor/secret"
)

type Params struct {
	Endpoint        string // RUNNER_ENDPOINT
	AccessKeyID     string // RUNNER_ACCESSKEYID
	SecretAccessKey string // RUNNER_SECRETACCESSKEY
	Location        string // RUNNER_LOCATION
	Token           string // RUNNER_TOKEN
	Ssl             bool   // RUNNER_SSL
	ScrapperEnabled bool   // RUNNER_SCRAPPERENABLED
	GitUsername     string // RUNNER_GITUSERNAME
	GitToken        string // RUNNER_GITTOKEN
	Datadir         string // RUNNER_DATADIR
}

func NewCypressRunner(dependency string) (*CypressRunner, error) {
	var params Params
	err := envconfig.Process("runner", &params)
	if err != nil {
		return nil, err
	}

	runner := &CypressRunner{
		Fetcher: content.NewFetcher(""),
		Scraper: scraper.NewMinioScraper(
			params.Endpoint,
			params.AccessKeyID,
			params.SecretAccessKey,
			params.Location,
			params.Token,
			params.Ssl,
		),
		Params:     params,
		dependency: dependency,
	}

	return runner, nil
}

// CypressRunner - implements runner interface used in worker to start test execution
type CypressRunner struct {
	Params     Params
	Fetcher    content.ContentFetcher
	Scraper    scraper.Scraper
	dependency string
}

func (r *CypressRunner) Run(execution testkube.Execution) (result testkube.ExecutionResult, err error) {
	// make some validation
	err = r.Validate(execution)
	if err != nil {
		return result, err
	}

	// check that the datadir exists
	_, err = os.Stat(r.Params.Datadir)
	if errors.Is(err, os.ErrNotExist) {
		return result, err
	}

	if execution.Content.IsFile() {
		output.PrintEvent("using file", execution)

		// TODO add cypress project structure
		// TODO checkout this repo with `skeleton` path
		// TODO overwrite skeleton/cypress/integration/test.js
		//      file with execution content git file
		return result, fmt.Errorf("passing cypress test as single file not implemented yet")
	}

	runPath := filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.Path)
	if execution.Content.Repository.WorkingDir != "" {
		runPath = filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.WorkingDir)
	}

	if _, err := os.Stat(filepath.Join(runPath, "package.json")); err == nil {
		// be gentle to different cypress versions, run from local npm deps
		if r.dependency == "npm" {
			out, err := executor.Run(runPath, "npm", nil, "install")
			if err != nil {
				return result, fmt.Errorf("npm install error: %w\n\n%s", err, out)
			}
		}

		if r.dependency == "yarn" {
			out, err := executor.Run(runPath, "yarn", nil, "install")
			if err != nil {
				return result, fmt.Errorf("yarn install error: %w\n\n%s", err, out)
			}
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if r.dependency == "npm" {
			out, err := executor.Run(runPath, "npm", nil, "init", "--yes")
			if err != nil {
				return result, fmt.Errorf("npm init error: %w\n\n%s", err, out)
			}

			out, err = executor.Run(runPath, "npm", nil, "install", "cypress", "--save-dev")
			if err != nil {
				return result, fmt.Errorf("npm install cypress error: %w\n\n%s", err, out)
			}
		}

		if r.dependency == "yarn" {
			out, err := executor.Run(runPath, "yarn", nil, "init", "--yes")
			if err != nil {
				return result, fmt.Errorf("yarn init error: %w\n\n%s", err, out)
			}

			out, err = executor.Run(runPath, "yarn", nil, "add", "cypress", "--dev")
			if err != nil {
				return result, fmt.Errorf("yarn add cypress error: %w\n\n%s", err, out)
			}
		}
	} else {
		return result, fmt.Errorf("checking package.json file: %w", err)
	}

	// handle project local Cypress version install (`Cypress` app)
	out, err := executor.Run(runPath, "./node_modules/cypress/bin/cypress", nil, "install")
	if err != nil {
		return result, fmt.Errorf("cypress binary install error: %w\n\n%s", err, out)
	}

	// convert executor env variables to os env variables
	for key, value := range execution.Envs {
		if err = os.Setenv(key, value); err != nil {
			return result, fmt.Errorf("setting env var: %w", err)
		}
	}

	envManager := secret.NewEnvManagerWithVars(execution.Variables)
	envManager.GetVars(execution.Variables)
	envVars := make([]string, 0, len(execution.Variables))
	for _, value := range execution.Variables {
		envVars = append(envVars, fmt.Sprintf("%s=%s", value.Name, value.Value))
	}

	projectPath := filepath.Join(r.Params.Datadir, "repo", execution.Content.Repository.Path)
	junitReportPath := filepath.Join(projectPath, "results/junit.xml")
	args := []string{"run", "--reporter", "junit", "--reporter-options", fmt.Sprintf("mochaFile=%s,toConsole=false", junitReportPath),
		"--env", strings.Join(envVars, ",")}

	if execution.Content.Repository.WorkingDir != "" {
		args = append(args, "--project", projectPath)
	}

	// append args from execution
	args = append(args, execution.Args...)

	// run cypress inside repo directory ignore execution error in case of failed test
	out, err = executor.Run(runPath, "./node_modules/cypress/bin/cypress", envManager, args...)
	out = envManager.Obfuscate(out)
	suites, serr := junit.IngestFile(junitReportPath)
	result = MapJunitToExecutionResults(out, suites)

	// scrape artifacts first even if there are errors above
	if r.Params.ScrapperEnabled {
		directories := []string{
			filepath.Join(projectPath, "cypress/videos"),
			filepath.Join(projectPath, "cypress/screenshots"),
		}
		err := r.Scraper.Scrape(execution.Id, directories)
		if err != nil {
			return result.WithErrors(fmt.Errorf("scrape artifacts error: %w", err)), nil
		}
	}

	return result.WithErrors(err, serr), nil
}

// Validate checks if Execution has valid data in context of Cypress executor
// Cypress executor runs currently only based on cypress project
func (r *CypressRunner) Validate(execution testkube.Execution) error {

	if execution.Content == nil {
		return fmt.Errorf("can't find any content to run in execution data: %+v", execution)
	}

	if execution.Content.Repository == nil {
		return fmt.Errorf("cypress executor handle only repository based tests, but repository is nil")
	}

	if execution.Content.Repository.Branch == "" && execution.Content.Repository.Commit == "" {
		return fmt.Errorf("can't find branch or commit in params, repo:%+v", execution.Content.Repository)
	}

	return nil
}

func MapJunitToExecutionResults(out []byte, suites []junit.Suite) (result testkube.ExecutionResult) {
	status := testkube.PASSED_ExecutionStatus
	result.Status = &status
	result.Output = string(out)
	result.OutputType = "text/plain"

	for _, suite := range suites {
		for _, test := range suite.Tests {

			result.Steps = append(
				result.Steps,
				testkube.ExecutionStepResult{
					Name:     fmt.Sprintf("%s - %s", suite.Name, test.Name),
					Duration: test.Duration.String(),
					Status:   MapStatus(test.Status),
				})
		}

		// TODO parse sub suites recursively

	}

	return result
}

func MapStatus(in junit.Status) (out string) {
	switch string(in) {
	case "passed":
		return string(testkube.PASSED_ExecutionStatus)
	default:
		return string(testkube.FAILED_ExecutionStatus)
	}
}

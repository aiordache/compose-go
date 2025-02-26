/*
   Copyright 2020 The Compose Specification Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cli

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/compose-spec/compose-go/errdefs"
	"github.com/compose-spec/compose-go/loader"
	"github.com/compose-spec/compose-go/types"
	"github.com/joho/godotenv"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ProjectOptions groups the command line options recommended for a Compose implementation
type ProjectOptions struct {
	Name        string
	WorkingDir  string
	ConfigPaths []string
	Environment map[string]string
	loadOptions []func(*loader.Options)
}

type ProjectOptionsFn func(*ProjectOptions) error

// NewProjectOptions creates ProjectOptions
func NewProjectOptions(configs []string, opts ...ProjectOptionsFn) (*ProjectOptions, error) {
	options := &ProjectOptions{
		ConfigPaths: configs,
		Environment: map[string]string{},
	}
	for _, o := range opts {
		err := o(options)
		if err != nil {
			return nil, err
		}
	}
	return options, nil
}

// WithName defines ProjectOptions' name
func WithName(name string) ProjectOptionsFn {
	return func(o *ProjectOptions) error {
		o.Name = name
		return nil
	}
}

// WithWorkingDirectory defines ProjectOptions' working directory
func WithWorkingDirectory(wd string) ProjectOptionsFn {
	return func(o *ProjectOptions) error {
		o.WorkingDir = wd
		return nil
	}
}

// WithEnv defines a key=value set of variables used for compose file interpolation
func WithEnv(env []string) ProjectOptionsFn {
	return func(o *ProjectOptions) error {
		for k, v := range getAsEqualsMap(env) {
			o.Environment[k] = v
		}
		return nil
	}
}

// WithDiscardEnvFiles sets discards the `env_file` section after resolving to
// the `environment` section
func WithDiscardEnvFile(o *ProjectOptions) error {
	o.loadOptions = append(o.loadOptions, loader.WithDiscardEnvFiles)
	return nil
}

// WithOsEnv imports environment variables from OS
func WithOsEnv(o *ProjectOptions) error {
	for k, v := range getAsEqualsMap(os.Environ()) {
		o.Environment[k] = v
	}
	return nil
}

// WithDotEnv imports environment variables from .env file
func WithDotEnv(o *ProjectOptions) error {
	dir, err := o.GetWorkingDir()
	if err != nil {
		return err
	}
	dotEnvFile := filepath.Join(dir, ".env")
	if _, err := os.Stat(dotEnvFile); os.IsNotExist(err) {
		return nil
	}
	file, err := os.Open(dotEnvFile)
	if err != nil {
		return err
	}
	defer file.Close()

	env, err := godotenv.Parse(file)
	if err != nil {
		return err
	}
	for k, v := range env {
		o.Environment[k] = v
	}
	return nil
}

// DefaultFileNames defines the Compose file names for auto-discovery (in order of preference)
var DefaultFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

const (
	ComposeProjectName   = "COMPOSE_PROJECT_NAME"
	ComposeFileSeparator = "COMPOSE_FILE_SEPARATOR"
	ComposeFilePath      = "COMPOSE_FILE"
)

func (o ProjectOptions) GetWorkingDir() (string, error) {
	if o.WorkingDir != "" {
		return o.WorkingDir, nil
	}
	for _, path := range o.ConfigPaths {
		if path != "-" {
			absPath, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			return filepath.Dir(absPath), nil
		}
	}
	return os.Getwd()
}

// ProjectFromOptions load a compose project based on command line options
func ProjectFromOptions(options *ProjectOptions) (*types.Project, error) {
	configPaths, specifiedComposeFiles, err := getConfigPathsFromOptions(options)
	if err != nil {
		return nil, err
	}

	configs, err := parseConfigs(configPaths)
	if err != nil {
		return nil, err
	}

	workingDir, err := options.GetWorkingDir()
	if err != nil {
		return nil, err
	}
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, err
	}

	var nameLoadOpt = func(opts *loader.Options) {
		if options.Name != "" {
			opts.Name = options.Name
		} else if nameFromEnv, ok := os.LookupEnv(ComposeProjectName); ok {
			opts.Name = nameFromEnv
		} else {
			opts.Name = regexp.MustCompile(`[^-_a-z0-9]+`).
				ReplaceAllString(strings.ToLower(filepath.Base(absWorkingDir)), "")
		}
	}
	options.loadOptions = append(options.loadOptions, nameLoadOpt)

	project, err := loader.Load(types.ConfigDetails{
		ConfigFiles: configs,
		WorkingDir:  workingDir,
		Environment: options.Environment,
	}, options.loadOptions...)
	if err != nil {
		return nil, err
	}

	project.ComposeFiles = specifiedComposeFiles
	return project, nil
}

// getConfigPathsFromOptions retrieves the config files for project based on project options
func getConfigPathsFromOptions(options *ProjectOptions) ([]string, []string, error) {
	paths := []string{}
	pwd := options.WorkingDir
	if pwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		pwd = wd
	}

	if len(options.ConfigPaths) != 0 {
		for _, f := range options.ConfigPaths {
			if f == "-" {
				paths = append(paths, f)
				continue
			}
			if !filepath.IsAbs(f) {
				f = filepath.Join(pwd, f)
			}
			if _, err := os.Stat(f); err != nil {
				return nil, nil, err
			}
			paths = append(paths, f)
		}
		return paths, options.ConfigPaths, nil
	}

	sep := os.Getenv(ComposeFileSeparator)
	if sep == "" {
		sep = string(os.PathListSeparator)
	}
	f := os.Getenv(ComposeFilePath)
	if f != "" {
		return strings.Split(f, sep), strings.Split(f, sep), nil
	}

	for {
		candidates := []string{}
		for _, n := range DefaultFileNames {
			f := filepath.Join(pwd, n)
			if _, err := os.Stat(f); err == nil {
				candidates = append(candidates, f)
			}
		}
		if len(candidates) > 0 {
			winner := candidates[0]
			if len(candidates) > 1 {
				logrus.Warnf("Found multiple config files with supported names: %s", strings.Join(candidates, ", "))
				logrus.Warnf("Using %s", winner)
			}
			return []string{winner}, []string{winner}, nil
		}
		parent := filepath.Dir(pwd)
		if parent == pwd {
			return nil, nil, errors.Wrap(errdefs.ErrNotFound, "can't find a suitable configuration file in this directory or any parent")
		}
		pwd = parent
	}
}

func parseConfigs(configPaths []string) ([]types.ConfigFile, error) {
	files := []types.ConfigFile{}
	for _, f := range configPaths {
		var (
			b   []byte
			err error
		)
		if f == "-" {
			b, err = ioutil.ReadAll(os.Stdin)
		} else {
			b, err = ioutil.ReadFile(f)
		}
		if err != nil {
			return nil, err
		}
		config, err := loader.ParseYAML(b)
		if err != nil {
			return nil, err
		}
		files = append(files, types.ConfigFile{Filename: f, Config: config})
	}
	return files, nil
}

// getAsEqualsMap split key=value formatted strings into a key : value map
func getAsEqualsMap(em []string) map[string]string {
	m := make(map[string]string)
	for _, v := range em {
		kv := strings.SplitN(v, "=", 2)
		m[kv[0]] = kv[1]
	}
	return m
}

// getAsEqualsMap format a key : value map into key=value strings
func getAsStringList(em map[string]string) []string {
	m := make([]string, 0, len(em))
	for k, v := range em {
		m = append(m, fmt.Sprintf("%s=%s", k, v))
	}
	return m
}

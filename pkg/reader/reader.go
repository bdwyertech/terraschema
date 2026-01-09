// (C) Copyright 2024 Hewlett Packard Enterprise Development LP

package reader

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/HewlettPackard/terraschema/pkg/model"
	"github.com/HewlettPackard/terraschema/pkg/registry"
	"github.com/HewlettPackard/terraschema/pkg/registry/regsrc"
	getter "github.com/hashicorp/go-getter"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
)

type RegistryClient struct {
	Client *registry.Client
	ctx    context.Context
	client *http.Client
}

func NewRegistryClient(ctx context.Context, client *http.Client) *RegistryClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &RegistryClient{
		Client: registry.NewClient(client),
		ctx:    ctx,
		client: client,
	}
}

// FetchModule downloads and extracts a module from the registry to a temp dir, returns local path.
func (rc *RegistryClient) FetchModule(address, version string) (*regsrc.Module, string, error) {
	src, err := regsrc.ParseModuleSource(address)
	if err != nil {
		return nil, "", err
	}
	if version == "" {
		versions, err := rc.Client.ModuleVersions(rc.ctx, src)
		if err != nil {
			return nil, "", err
		}
		if len(versions.Modules) > 0 && len(versions.Modules[0].Versions) > 0 {
			latest := versions.Modules[0].Versions[0].Version
			for _, ver := range versions.Modules[0].Versions {
				if compareVersions(ver.Version, latest) > 0 {
					latest = ver.Version
				}
			}
			version = latest
		}
	}
	mod, err := rc.Client.ModuleLocation(rc.ctx, src, version)
	if err != nil {
		return src, "", err
	}

	tmpDir, err := os.MkdirTemp("", "tfmod-*")
	if err != nil {
		return src, "", err
	}
	log.Debugln(src)
	log.Debugln(mod)
	client := &getter.Client{
		Ctx:  rc.ctx,
		Src:  mod,
		Dst:  tmpDir,
		Mode: getter.ClientModeAny,
	}
	return src, tmpDir, client.Get()
}

// RemoteRegistryClient and support for fetching Terraform modules from remote registries
// (initial interface and stub, implementation to follow)

type ModuleSourceType int

const (
	ModuleSourceLocalDir ModuleSourceType = iota
	ModuleSourceRemoteRegistry
)

type ModuleSource struct {
	Type    ModuleSourceType
	Path    string // local path or registry address
	Version string // for remote registry
}

type RemoteRegistryClient interface {
	// FetchModule downloads and extracts a module from the registry to a temp dir, returns local path
	FetchModule(address, version string) (string, error)
}

var fileSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type:       "variable",
			LabelNames: []string{"name"},
		},
	},
}

var (
	ErrFilesNotFound    = fmt.Errorf("no .tf files found")
	ErrNoVariablesFound = fmt.Errorf("tf files don't contain any variables")
)

// GetVarMap reads all .tf files in a directory and returns a map of variable names to their translated values.
// For the purpose of this application, all that matters is the model.VariableBlock contained in this, which
// contains a direct unmarshal of the block itself using the hcl package. The rest of the information is for
// debugging purposes, and to simplify the process of deciding if a variable is 'required' later. Note: in 'strict'
// mode, all variables are required, regardless of whether they have a default value or not.
func GetVarMap(path string, debugOut bool) (map[string]model.TranslatedVariable, error) {
	// read all tf files in directory
	files, err := filepath.Glob(filepath.Join(path, "*.tf"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, ErrFilesNotFound
	}

	if debugOut {
		fmt.Printf("Debug: found the following files in %q:\n", path)
	}

	parser := hclparse.NewParser()

	varMap := make(map[string]model.TranslatedVariable)
	for _, fileName := range files {
		if debugOut {
			fmt.Printf("\t%q, with variable(s):\n", fileName)
		}

		file, d := parser.ParseHCLFile(fileName)
		if d.HasErrors() {
			return nil, d
		}

		blocks, _, d := file.Body.PartialContent(fileSchema)
		if d.HasErrors() {
			return nil, d
		}
		for _, block := range blocks.Blocks {
			name, translated, err := getTranslatedVariableFromBlock(block, file)
			if err != nil {
				return nil, fmt.Errorf("error getting parsing %q: %w", name, err)
			}
			varMap[name] = translated

			if debugOut {
				fmt.Printf("\t\t%s\n", name)
			}
		}
	}

	if len(varMap) == 0 {
		return nil, ErrNoVariablesFound
	}

	return varMap, nil
}

func getTranslatedVariableFromBlock(block *hcl.Block, file *hcl.File) (string, model.TranslatedVariable, error) {
	name := block.Labels[0]
	variable := model.VariableBlock{}
	d := gohcl.DecodeBody(block.Body, nil, &variable)
	if d.HasErrors() {
		return name, model.TranslatedVariable{}, d
	}

	variable.Default = filterMissingExpression(variable.Default)
	variable.Type = filterMissingExpression(variable.Type)

	out := model.TranslatedVariable{Variable: variable, Required: true}

	// Get type, default, and condition as strings and add them to the translated variable struct.
	// This is to make the code easier to debug, since hcl.Expressions are difficult to read out of context.

	// check if 'default' exists
	if variable.Default != nil {
		defaultAsString := printToString(variable.Default, file)
		out.DefaultAsString = &defaultAsString
		out.Required = false
	}

	// check if 'type' exists
	if variable.Type != nil {
		typeAsString := printToString(variable.Type, file)
		out.TypeAsString = &typeAsString
	}

	// plaintext print all condition expressions into the ConditionsAsString field.
	out.ConditionsAsString = make([]string, len(variable.Validations))
	for i, validation := range variable.Validations {
		out.ConditionsAsString[i] = printToString(validation.Condition, file)
	}

	return name, out, nil
}

func filterMissingExpression(in hcl.Expression) hcl.Expression {
	// if the start and the end range are the same, this means the field is not
	// real, so it can be removed.
	if in.Range().Start.Byte == in.Range().End.Byte {
		return nil
	}

	return in
}

func printToString(in hcl.Expression, f *hcl.File) string {
	out := string(in.Range().SliceBytes(f.Bytes))

	return out
}

// compareVersions compares two semantic version strings
// Returns: 1 if v1 > v2, -1 if v1 < v2, 0 if equal
func compareVersions(v1, v2 string) int {
	// Remove 'v' prefix if present
	if len(v1) > 0 && v1[0] == 'v' {
		v1 = v1[1:]
	}
	if len(v2) > 0 && v2[0] == 'v' {
		v2 = v2[1:]
	}
	
	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")
	
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}
	
	for i := 0; i < maxLen; i++ {
		var p1, p2 int
		if i < len(parts1) {
			p1, _ = strconv.Atoi(parts1[i])
		}
		if i < len(parts2) {
			p2, _ = strconv.Atoi(parts2[i])
		}
		
		if p1 > p2 {
			return 1
		} else if p1 < p2 {
			return -1
		}
	}
	return 0
}

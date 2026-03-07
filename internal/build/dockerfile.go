package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Dockerfile templates — ported from apps/api/app/infrastructure/build/dockerfile_generator.py

// detectPackageManager returns the Node.js package manager based on lockfiles.
func detectPackageManager(contextPath string) string {
	if fileExists(filepath.Join(contextPath, "bun.lockb")) || fileExists(filepath.Join(contextPath, "bun.lock")) {
		return "bun"
	}
	if fileExists(filepath.Join(contextPath, "pnpm-lock.yaml")) {
		return "pnpm"
	}
	if fileExists(filepath.Join(contextPath, "yarn.lock")) {
		return "yarn"
	}
	return "npm"
}

func copyDepsForPM(pm string) string {
	switch pm {
	case "pnpm":
		return "package.json pnpm-lock.yaml* ./"
	case "yarn":
		return "package.json yarn.lock* ./"
	case "bun":
		return "package.json bun.lockb* ./"
	default:
		return "package*.json ./"
	}
}

func installCmdForPM(pm string, production bool) string {
	switch pm {
	case "pnpm":
		cmd := "corepack enable && pnpm install --frozen-lockfile"
		if production {
			return cmd + " --prod"
		}
		return cmd
	case "yarn":
		cmd := "yarn install --frozen-lockfile"
		if production {
			return cmd + " --production"
		}
		return cmd
	case "bun":
		cmd := "bun install"
		if production {
			return cmd + " --production"
		}
		return cmd
	default:
		if production {
			return "npm ci --omit=dev"
		}
		return "npm ci"
	}
}

func buildNodeSPADockerfile(pm, buildDir string) string {
	return fmt.Sprintf(`FROM node:20-alpine AS build
WORKDIR /app
COPY %s
RUN %s
COPY . .
RUN %s run build

FROM nginx:alpine
COPY --from=build /app/%s /usr/share/nginx/html
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
`, copyDepsForPM(pm), installCmdForPM(pm, false), pm, buildDir)
}

func buildNodeServerDockerfile(pm string, port int) string {
	return fmt.Sprintf(`FROM node:20-alpine
WORKDIR /app
COPY %s
RUN %s
COPY . .
EXPOSE %d
CMD ["%s", "start"]
`, copyDepsForPM(pm), installCmdForPM(pm, true), port, pm)
}

const pythonRequirementsDockerfile = `FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
EXPOSE %d
CMD ["python", "-m", "uvicorn", "%s", "--host", "0.0.0.0", "--port", "%d"]
`

const pythonPyprojectDockerfile = `FROM python:3.12-slim
WORKDIR /app
COPY pyproject.toml ./
COPY . .
RUN pip install --no-cache-dir .
EXPOSE %d
CMD ["python", "-m", "uvicorn", "%s", "--host", "0.0.0.0", "--port", "%d"]
`

const goDockerfile = `FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=build /server /server
EXPOSE %d
CMD ["/server"]
`

const staticHTMLDockerfile = `FROM nginx:alpine
COPY . /usr/share/nginx/html
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]
`

// GenerateDockerfileResult holds the output of auto-Dockerfile detection.
type GenerateDockerfileResult struct {
	Generated     bool
	EffectivePort int
	HealthPath    string
}

// GenerateDockerfileIfMissing generates a Dockerfile if one doesn't exist.
func GenerateDockerfileIfMissing(contextPath, dockerfilePath string, port int) GenerateDockerfileResult {
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	fullPath := filepath.Join(contextPath, dockerfilePath)
	if fileExists(fullPath) {
		return GenerateDockerfileResult{Generated: false, EffectivePort: port, HealthPath: "/health"}
	}

	content, effectivePort, healthPath := detectProject(contextPath, port)
	if content == "" {
		return GenerateDockerfileResult{Generated: false, EffectivePort: port, HealthPath: "/health"}
	}

	// Ensure parent directory exists
	dir := filepath.Dir(fullPath)
	_ = os.MkdirAll(dir, 0o755)

	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return GenerateDockerfileResult{Generated: false, EffectivePort: port, HealthPath: "/health"}
	}

	return GenerateDockerfileResult{
		Generated:     true,
		EffectivePort: effectivePort,
		HealthPath:    healthPath,
	}
}

// isDesktopApp checks build script content, config files, and deps to detect
// desktop/mobile apps (Electron, Tauri, React Native) that can't be served as
// web applications in Docker.
func isDesktopApp(contextPath string, pkg map[string]interface{}) bool {
	// Layer 1: Build script content (most direct — this is what actually fails)
	if scripts, ok := pkg["scripts"].(map[string]interface{}); ok {
		if buildScript, ok := scripts["build"].(string); ok {
			desktopCmds := []string{
				"electron-builder", "electron-forge", "electron-packager",
				"tauri build", "react-native bundle", "expo build", "expo export",
			}
			for _, cmd := range desktopCmds {
				if strings.Contains(buildScript, cmd) {
					return true
				}
			}
		}
	}

	// Layer 2: Config files (definitive presence)
	configFiles := []string{
		"electron-builder.yml", "electron-builder.json5", "electron-builder.json",
		"forge.config.js", "forge.config.ts", "forge.config.cjs",
		"tauri.conf.json",
	}
	for _, f := range configFiles {
		if fileExists(filepath.Join(contextPath, f)) {
			return true
		}
	}

	// Layer 3: Dependency names (broadest catch)
	markers := []string{
		"electron", "electron-builder", "electron-packager", "@electron-forge/cli",
		"react-native", "@tauri-apps/cli", "@tauri-apps/api",
	}
	for _, m := range markers {
		if hasDep(pkg, m) {
			return true
		}
	}

	return false
}

func detectProject(contextPath string, port int) (string, int, string) {
	// Node.js
	pkg := readPackageJSON(contextPath)
	if pkg != nil {
		// Check for desktop/mobile apps — don't generate a Dockerfile
		if isDesktopApp(contextPath, pkg) {
			return "", 0, ""
		}
		// SPA first
		if content, ePort, hPath := detectNodeSPA(contextPath, pkg); content != "" {
			return content, ePort, hPath
		}
		// Node server
		if scripts, ok := pkg["scripts"].(map[string]interface{}); ok {
			if _, hasStart := scripts["start"]; hasStart {
				pm := detectPackageManager(contextPath)
				return buildNodeServerDockerfile(pm, port), port, "/health"
			}
		}
	}

	// Python
	if fileExists(filepath.Join(contextPath, "requirements.txt")) {
		entrypoint := detectPythonEntrypoint(contextPath)
		return fmt.Sprintf(pythonRequirementsDockerfile, port, entrypoint, port), port, "/"
	}
	if fileExists(filepath.Join(contextPath, "pyproject.toml")) {
		entrypoint := detectPythonEntrypoint(contextPath)
		return fmt.Sprintf(pythonPyprojectDockerfile, port, entrypoint, port), port, "/"
	}

	// Go
	if fileExists(filepath.Join(contextPath, "go.mod")) {
		return fmt.Sprintf(goDockerfile, port), port, "/health"
	}

	// Static HTML (generic catch-all)
	if fileExists(filepath.Join(contextPath, "index.html")) {
		return staticHTMLDockerfile, 80, "/"
	}

	return "", 0, ""
}

func readPackageJSON(contextPath string) map[string]interface{} {
	data, err := os.ReadFile(filepath.Join(contextPath, "package.json"))
	if err != nil {
		return nil
	}
	var pkg map[string]interface{}
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	return pkg
}

func hasDep(pkg map[string]interface{}, name string) bool {
	for _, key := range []string{"dependencies", "devDependencies"} {
		if deps, ok := pkg[key].(map[string]interface{}); ok {
			if _, exists := deps[name]; exists {
				return true
			}
		}
	}
	return false
}

func detectNodeSPA(contextPath string, pkg map[string]interface{}) (string, int, string) {
	spaMarkers := []string{"vite", "next", "@vitejs/plugin-react", "react-scripts", "@vue/cli-service", "nuxt"}
	found := false
	for _, m := range spaMarkers {
		if hasDep(pkg, m) {
			found = true
			break
		}
	}
	if !found {
		return "", 0, ""
	}

	scripts, ok := pkg["scripts"].(map[string]interface{})
	if !ok {
		return "", 0, ""
	}
	if _, hasBuild := scripts["build"]; !hasBuild {
		return "", 0, ""
	}

	pm := detectPackageManager(contextPath)

	buildDir := "dist"
	if hasDep(pkg, "next") {
		buildDir = "out"
	} else if hasDep(pkg, "react-scripts") {
		buildDir = "build"
	}

	return buildNodeSPADockerfile(pm, buildDir), 80, "/"
}

var appPatternRe = regexp.MustCompile(`(?m)^\s*app\s*=\s*(FastAPI|Flask|Starlette)\(`)

func detectPythonEntrypoint(contextPath string) string {
	candidates := []string{"main.py", "app.py", "app/main.py", "app/__init__.py"}
	for _, candidate := range candidates {
		fpath := filepath.Join(contextPath, candidate)
		data, err := os.ReadFile(fpath)
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 4096 {
			content = content[:4096]
		}
		if appPatternRe.MatchString(content) {
			module := strings.ReplaceAll(candidate, "/", ".")
			module = strings.TrimSuffix(module, ".py")
			module = strings.TrimSuffix(module, ".__init__")
			return module + ":app"
		}
	}
	return "main:app"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

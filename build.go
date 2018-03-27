// +build ignore

package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/karlmutch/duat"
	"github.com/karlmutch/duat/version"

	"github.com/karlmutch/errors" // Forked copy of https://github.com/jjeffery/errors
	"github.com/karlmutch/stack"  // Forked copy of https://github.com/go-stack/stack
	"github.com/mgutz/logxi"      // Using a forked copy of this package results in build issues

	"github.com/karlmutch/envflag" // Forked copy of https://github.com/GoBike/envflag
)

var (
	logger = logxi.New("build.go")

	prune       bool
	verbose     bool
	recursive   bool
	userDirs    string
	imageOnly   bool
	githubToken string
)

func init() {
	flag.BoolVar(&prune, "prune", true, "When enabled will prune any prerelease images replaced by this build")
	flag.BoolVar(&verbose, "v", false, "When enabled will print internal logging for this tool")
	flag.BoolVar(&recursive, "r", false, "When enabled this tool will visit any sub directories that contain main functions and build in each")
	flag.StringVar(&userDirs, "dirs", ".", "A comma seperated list of root directories that will be used a starting points looking for Go code, this will default to the current working directory")
	flag.BoolVar(&imageOnly, "image-only", false, "Used to start at the docker build step, will progress to github release, if not set the build halts after compilation")
	flag.StringVar(&githubToken, "github-token", "", "If set this will automatically trigger a release of the binary artifacts to github at the current version")
}

func usage() {
	fmt.Fprintln(os.Stderr, path.Base(os.Args[0]))
	fmt.Fprintln(os.Stderr, "usage: ", os.Args[0], "[options]       build tool (build.go)      ", version.GitHash, "    ", version.BuildTime)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Arguments")
	fmt.Fprintln(os.Stderr, "")
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Environment Variables:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "options can also be extracted from environment variables by changing dashes '-' to underscores and using upper case.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "log levels are handled by the LOGXI env variables, these are documented at https://github.com/mgutz/logxi")
}

func init() {
	flag.Usage = usage
}

func main() {
	// This code is run in the same fashion as a script and should be co-located
	// with the component that is being built

	// Parse the CLI flags
	if !flag.Parsed() {
		envflag.Parse()
	}

	if verbose {
		logger.SetLevel(logxi.LevelDebug)
	}

	// First assume that the directory supplied is a code directory
	rootDirs := strings.Split(userDirs, ",")
	dirs := []string{}

	err := errors.New("")

	// If this is a recursive build scan all inner directories looking for go code
	// and build it if there is code found
	//
	if recursive {
		for _, dir := range rootDirs {
			// Will auto skip any vendor directories found
			found, err := duat.FindGoDirs(dir)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				os.Exit(-1)
			}
			dirs = append(dirs, found...)
		}
	} else {
		dirs = rootDirs
	}

	logger.Debug(fmt.Sprintf("%v", dirs))

	// Take the discovered directories and build them
	//
	outputs := []string{}
	localOut := []string{}

	for _, dir := range dirs {
		localOut, err = runBuild(dir, "README.md")
		outputs = append(outputs, localOut...)
		if err != nil {
			break
		}
	}

	for _, output := range outputs {
		fmt.Fprintln(os.Stdout, output)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(-2)
	}
}

// runBuild is used to restore the current working directory after the build itself
// has switch directories
//
func runBuild(dir string, verFn string) (outputs []string, err errors.Error) {

	logger.Info(fmt.Sprintf("processing %s", dir))

	cwd, errGo := os.Getwd()
	if errGo != nil {
		return outputs, errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
	}

	// Gather information about the current environment. also changes directory to the working area
	md, err := duat.NewMetaData(dir, verFn)
	if err != nil {
		return outputs, err
	}

	// Are we running inside a container runtime such as docker
	runtime, err := md.ContainerRuntime()
	if err != nil {
		return nil, err
	}

	// If we are in a container then do a stock compile, if not then it is
	// time to dockerize all the things
	built := []string{}
	if len(runtime) != 0 {
		logger.Info(fmt.Sprintf("building %s", dir))
		if outputs, err = build(md); err == nil {
			built = append(built, outputs...)
		}
	} else {
		logger.Info(fmt.Sprintf("dockerizing %s", dir))
		outputs, err = dockerize(md)

		// If we dockerized successfully then place the binary file names
		// into the output list for the github release step
		if err == nil {
			built, err = md.GoFetchBuilt()
		}
	}

	if err == nil && len(githubToken) != 0 {
		logger.Info(fmt.Sprintf("github releasing %s", dir))
		err = md.CreateRelease(githubToken, "", built)
	}

	if errGo = os.Chdir(cwd); errGo != nil {
		logger.Warn("The original directory could not be restored after the build completed")
		if err == nil {
			err = errors.Wrap(errGo).With("stack", stack.Trace().TrimRuntime())
		}
	}

	return outputs, err
}

// build performs the default build for the component within the directory specified, but does
// no further than producing binaries that need to be done within a isolated container
//
func build(md *duat.MetaData) (outputs []string, err errors.Error) {
	return md.GoBuild()
}

// dockerize is used to produce containers where appropriate within a build
// target directory
//
func dockerize(md *duat.MetaData) (outputs []string, err errors.Error) {

	exists, _, err := md.ImageExists()

	output := strings.Builder{}
	if !exists {
		err = md.ImageCreate(&output)
	}
	return strings.Split(output.String(), "\n"), err
}
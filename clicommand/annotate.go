package clicommand

import (
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/buildkite/agent/agent"
	"github.com/buildkite/agent/api"
	"github.com/buildkite/agent/cliconfig"
	"github.com/buildkite/agent/logger"
	"github.com/buildkite/agent/retry"
	"github.com/urfave/cli"
	"golang.org/x/crypto/ssh/terminal"
)

var AnnotateHelpDescription = `Usage:

   buildkite-agent annotate <file> [arguments...]

Description:

   Build annotations allow you to customize the Buildkite build interface to
   show information that may surface from your builds. Some examples include:

   - Links to artifacts generated by your jobs
   - Test result summaries
   - Graphs that include analysis about your codebase
   - Helpful information for team members about what happened during a build

   Annotations can be written in either Markdown or HTML.

   You can update an existing annotation's body by running the annotate command
   again and provide the same context as the one you want to update. Or if you
   leave context blank, it will use the default context.

   You can also just update the style of an existing annotation by omitting the
   body and just providing a new style value.

Example:

   $ buildkite-agent annotate "All tests passed! :rocket:"
   $ cat annotation.md | buildkite-agent annotate --style "warning"
   $ buildkite-agent annotate --style "success" --context "junit"
   $ ./script/dynamic_annotation_generator | buildkite-agent annotate --style "success"`

type AnnotateConfig struct {
	Body             string `cli:"arg:0" label:"annotation body"`
	Style            string `cli:"style"`
	Context          string `cli:"context"`
	Append           bool   `cli:"append"`
	Job              string `cli:"job" validate:"required"`
	AgentAccessToken string `cli:"agent-access-token" validate:"required"`
	Endpoint         string `cli:"endpoint" validate:"required"`
	NoColor          bool   `cli:"no-color"`
	Debug            bool   `cli:"debug"`
	DebugHTTP        bool   `cli:"debug-http"`
}

var AnnotateCommand = cli.Command{
	Name:        "annotate",
	Usage:       "Annotate the build page within the Buildkite UI with text from within a Buildkite job",
	Description: AnnotateHelpDescription,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:   "context",
			Usage:  "The context of the annotation used to differentiate this annotation from others",
			EnvVar: "BUILDKITE_ANNOTATION_CONTEXT",
		},
		cli.StringFlag{
			Name:   "style",
			Usage:  "The style of the annotation (`success`, `info`, `warning` or `error`)",
			EnvVar: "BUILDKITE_ANNOTATION_STYLE",
		},
		cli.BoolFlag{
			Name:   "append",
			Usage:  "Append to the body of an existing annotation",
			EnvVar: "BUILDKITE_ANNOTATION_APPEND",
		},
		cli.StringFlag{
			Name:   "job",
			Value:  "",
			Usage:  "Which job should the annotation come from",
			EnvVar: "BUILDKITE_JOB_ID",
		},
		AgentAccessTokenFlag,
		EndpointFlag,
		NoColorFlag,
		DebugFlag,
		DebugHTTPFlag,
	},
	Action: func(c *cli.Context) {
		// The configuration will be loaded into this struct
		cfg := AnnotateConfig{}

		// Load the configuration
		loader := cliconfig.Loader{CLI: c, Config: &cfg}
		if err := loader.Load(); err != nil {
			logger.Fatal("%s", err)
		}

		// Setup the any global configuration options
		HandleGlobalFlags(cfg)

		var body string
		var err error

		if cfg.Body != "" {
			body = cfg.Body
		} else if !terminal.IsTerminal(int(os.Stdin.Fd())) {
			logger.Info("Reading annotation body from STDIN")

			// Actually read the file from STDIN
			stdin, err := ioutil.ReadAll(os.Stdin)
			if err != nil {
				logger.Fatal("Failed to read from STDIN: %s", err)
			}

			body = string(stdin[:])
		}

		// Trim any whitespace edges on the annotation body
		body = strings.TrimSpace(body)

		// Create the API client
		client := agent.APIClient{
			Endpoint: cfg.Endpoint,
			Token:    cfg.AgentAccessToken,
		}.Create()

		// Create the annotation we'll send to the Buildkite API
		annotation := &api.Annotation{
			Body:    body,
			Style:   cfg.Style,
			Context: cfg.Context,
			Append:  cfg.Append,
		}

		// Retry the annotation a few times before giving up
		err = retry.Do(func(s *retry.Stats) error {
			// Attempt ot create the annotation
			resp, err := client.Annotations.Create(cfg.Job, annotation)

			// Don't bother retrying if the response was one of these statuses
			if resp != nil && (resp.StatusCode == 401 || resp.StatusCode == 404 || resp.StatusCode == 400) {
				s.Break()
				return err
			}

			// Show the unexpected error
			if err != nil {
				logger.Warn("%s (%s)", err, s)
			}

			return err
		}, &retry.Config{Maximum: 5, Interval: 1 * time.Second, Jitter: true})

		// Show a fatal error if we gave up trying to create the annotation
		if err != nil {
			logger.Fatal("Failed to annotate build: %s", err)
		}

		logger.Info("Successfully annotated build")
	},
}
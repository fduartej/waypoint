package ecrpull

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aws/aws-sdk-go/aws"
	awsecr "github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/hashicorp/waypoint-plugin-sdk/docs"
	"github.com/hashicorp/waypoint-plugin-sdk/terminal"
	"github.com/hashicorp/waypoint/builtin/aws/ecr"
	"github.com/hashicorp/waypoint/builtin/aws/utils"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// Builder uses `docker build` to build a Docker iamge.
type Builder struct {
	config Config
}

// BuildFunc implements component.Builder
func (b *Builder) BuildFunc() interface{} {
	return b.Build
}

// Config is the configuration structure for the registry.
type Config struct {
	Region            string `hcl:"region,optional"`
	Repository        string `hcl:"repository,attr"`
	Tag               string `hcl:"tag,attr"`
	ForceArchitecture string `hcl:"force_architecture,optional"`
}

func (b *Builder) Documentation() (*docs.Documentation, error) {
	doc, err := docs.New(docs.FromConfig(&Config{}), docs.FromFunc(b.BuildFunc()))
	if err != nil {
		return nil, err
	}

	doc.Description(`
Use an existing, pre-built AWS ECR image.

This builder attempts to find an image by repository and tag in the
specified region. If found, it will pass along the image information
to the next step.

This builder will not modify the image. 

If you wish to rename or retag an image, please use the "docker-pull" component
in conjunction with the "aws-ecr" registry option.
`)

	doc.Example(`
build {
  use "aws-ecr-pull" {
    region     = "us-east-1"
    repository = "deno-http"
    tag        = "latest"
  }
}
`)

	doc.Input("component.Source")
	doc.Output("ecr.Image")

	doc.SetField(
		"region",
		"the AWS region the ECR repository is in",
		docs.Summary(
			"if not set uses the environment variable AWS_REGION or AWS_REGION_DEFAULT.",
		),
	)

	doc.SetField(
		"repository",
		"the AWS ECR repository name",
	)

	doc.SetField(
		"tag",
		"the tag of the image to pull",
	)

	doc.SetField(
		"force_architecture",
		"**Note**: This is a temporary field that enables overriding the `architecture` output attribute. Valid values are: `\"x86_64\"`, `\"arm64\"`",
		docs.Default("`\"\"`"),
	)

	return doc, nil
}

// ConfigSet is called after a configuration has been decoded
func (p *Builder) ConfigSet(config interface{}) error {
	c, ok := config.(*Config)
	if !ok {
		// this should never happen
		return fmt.Errorf("Invalid configuration, expected *ecrpull.BuilderConfig, got %q", reflect.TypeOf(config))
	}

	// validate required fields
	if err := utils.Error(validation.ValidateStruct(c,
		validation.Field(&c.Repository, validation.Required),
		validation.Field(&c.Tag, validation.Required),
	)); err != nil {
		return err
	}

	// validate architecture
	if c.ForceArchitecture != "" {
		architectures := make([]interface{}, len(lambda.Architecture_Values()))

		for i, ca := range lambda.Architecture_Values() {
			architectures[i] = ca
		}

		var validArchitectures []string
		for _, arch := range lambda.Architecture_Values() {
			validArchitectures = append(validArchitectures, fmt.Sprintf("%q", arch))
		}

		if err := utils.Error(validation.ValidateStruct(c,
			validation.Field(&c.ForceArchitecture,
				validation.In(architectures...).Error(fmt.Sprintf("Unsupported force_architecture %q. Must be one of [%s], or left blank", c.ForceArchitecture, strings.Join(validArchitectures, ", "))),
			),
		)); err != nil {
			return err
		}
	}

	return nil
}

// Config implements Configurable
func (b *Builder) Config() (interface{}, error) {
	return &b.config, nil
}

// Build
func (b *Builder) Build(ctx context.Context, ui terminal.UI, log hclog.Logger) (*ecr.Image, error) {

	// If there is no region setup. Try and load it from environment variables.
	if b.config.Region == "" {
		b.config.Region = os.Getenv("AWS_REGION")

		if b.config.Region == "" {
			b.config.Region = os.Getenv("AWS_REGION_DEFAULT")
		}
	}

	if b.config.Region == "" {
		return nil, status.Error(
			codes.FailedPrecondition,
			"Please set your aws region in the deployment config, or set the environment variable 'AWS_REGION' or 'AWS_DEFAULT_REGION'")
	}

	sg := ui.StepGroup()
	step := sg.Add("")
	defer func() {
		if step != nil {
			step.Abort()
		}
	}()

	// connect to AWS
	step.Update("Connecting to AWS")
	sess, err := utils.GetSession(&utils.SessionConfig{
		Region: b.config.Region,
		Logger: log,
	})

	if err != nil {
		log.Error("error connecting to AWS", "error", err)
		return nil, err
	}

	step.Done()

	// find ECR image by repository and tag
	step = sg.Add("Verifying image exists")
	ecrsvc := awsecr.New(sess)

	cfgTag := b.config.Tag
	cfgRepository := b.config.Repository

	// should be acceptable to filter images by TAGGED status
	imgs, err := ecrsvc.DescribeImages(&awsecr.DescribeImagesInput{
		RepositoryName: aws.String(cfgRepository),
		Filter: &awsecr.DescribeImagesFilter{
			TagStatus: aws.String("TAGGED"),
		},
	})
	if err != nil {
		log.Error("error describing images", "error", err, "repository", cfgRepository)
		return nil, err
	}

	if len(imgs.ImageDetails) == 0 {
		log.Error("no tagged images found", "repository", cfgRepository)
		return nil, status.Error(codes.FailedPrecondition, "No images found")
	}
	log.Debug("found images", "image count", len(imgs.ImageDetails))

	var output ecr.Image
	for _, img := range imgs.ImageDetails {
		for _, tag := range img.ImageTags {
			if *tag == cfgTag {
				// an image with the specified tag was found
				imageMatch := *img.RegistryId + ".dkr.ecr." + b.config.Region + ".amazonaws.com/" + cfgRepository

				output.Image = imageMatch
				output.Tag = *tag

				st := ui.Status()
				defer st.Close()

				if b.config.ForceArchitecture != "" {
					output.Architecture = b.config.ForceArchitecture
					st.Step(terminal.StatusOK, "Forcing output architecture: "+b.config.ForceArchitecture)
				} else {
					// TODO(kevinwang): Do we need to get architecture?
					// If we do, we can pull the image and inspect it via `cli.ImageInspectWithRaw`,
					// - prior art: /builtin/docker/builder.go -> Build
					// There is also an open issue for the ECR team to build a architecture feature into
					// the UI, which probably comes with a CLI/API change.
					// - see https://github.com/aws/containers-roadmap/issues/1591
					st.Step(terminal.StatusWarn, "Automatic architecture detection is not yet implemented. Architecture will default to \"\"")
				}

				break
			}
		}

	}

	// if no image was found, return an error
	if output.Image == "" {
		log.Error("no matching image was found", "tag", cfgTag, "repository", cfgRepository)
		return nil, status.Error(codes.FailedPrecondition, "No matching tags found")
	}

	step.Update("Using image: " + output.Image + ":" + output.Tag)
	step.Done()

	return &output, nil
}

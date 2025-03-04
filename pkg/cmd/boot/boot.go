package boot

import (
	"fmt"
	"os"
	"path/filepath"

	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/jenkins-x/jx/pkg/cmd/helper"
	"github.com/jenkins-x/jx/pkg/cmd/namespace"
	"github.com/jenkins-x/jx/pkg/cmd/step/create"
	"github.com/jenkins-x/jx/pkg/config"
	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/pkg/errors"

	"github.com/spf13/cobra"

	"github.com/jenkins-x/jx/pkg/cmd/opts"
	"github.com/jenkins-x/jx/pkg/cmd/templates"
)

// BootOptions options for the command
type BootOptions struct {
	*opts.CommonOptions

	Dir          string
	GitURL       string
	GitRef       string
	StartStep    string
	EndStep      string
	HelmLogLevel string

	// The bootstrap URL for the version stream. Once we have a jx-requirements.yaml files, we read that
	VersionStreamURL string
	// The bootstrap ref for the version stream. Once we have a jx-requirements.yaml, we read that
	VersionStreamRef string
}

var (
	bootLong = templates.LongDesc(`
		Boots up Jenkins X in a Kubernetes cluster using GitOps and a Jenkins X Pipeline

		For more documentation see: [https://jenkins-x.io/getting-started/boot/](https://jenkins-x.io/getting-started/boot/)

`)

	bootExample = templates.Examples(`
		# create a kubernetes cluster via Terraform or via jx
		jx create cluster gke --skip-installation

		# now lets boot up Jenkins X installing/upgrading whatever is needed
		jx boot 

		# if we have already booted and just want to apply some environment changes without 
        # re-applying ingress and so forth we can start at the environment step:
		jx boot --start-step install-env
`)
)

// NewCmdBoot creates the command
func NewCmdBoot(commonOpts *opts.CommonOptions) *cobra.Command {
	options := &BootOptions{
		CommonOptions: commonOpts,
	}
	cmd := &cobra.Command{
		Use:     "boot",
		Aliases: []string{"bootstrap"},
		Short:   "Boots up Jenkins X in a Kubernetes cluster using GitOps and a Jenkins X Pipeline",
		Long:    bootLong,
		Example: bootExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}

	cmd.Flags().StringVarP(&options.Dir, "dir", "d", ".", "the directory to look for the Jenkins X Pipeline, requirements and charts")
	cmd.Flags().StringVarP(&options.GitURL, "git-url", "u", "", "override the Git clone URL for the JX Boot source to start from, ignoring the versions stream. Normally specified with git-ref as well")
	cmd.Flags().StringVarP(&options.GitRef, "git-ref", "", "", "override the Git ref for the JX Boot source to start from, ignoring the versions stream. Normally specified with git-url as well")
	cmd.Flags().StringVarP(&options.VersionStreamURL, "versions-repo", "", config.DefaultVersionsURL, "the bootstrap URL for the versions repo. Once the boot config is cloned, the repo will be then read from the jx-requirements.yaml")
	cmd.Flags().StringVarP(&options.VersionStreamRef, "versions-ref", "", config.DefaultVersionsRef, "the bootstrap ref for the versions repo. Once the boot config is cloned, the repo will be then read from the jx-requirements.yaml")
	cmd.Flags().StringVarP(&options.StartStep, "start-step", "s", "", "the step in the pipeline to start from")
	cmd.Flags().StringVarP(&options.EndStep, "end-step", "e", "", "the step in the pipeline to end at")
	cmd.Flags().StringVarP(&options.HelmLogLevel, "helm-log", "v", "", "sets the helm logging level from 0 to 9. Passed into the helm CLI via the '-v' argument. Useful to diagnose helm related issues")
	return cmd
}

// Run runs this command
func (o *BootOptions) Run() error {
	info := util.ColorInfo

	err := o.verifyClusterConnection()
	if err != nil {
		return err
	}

	o.overrideSteps()

	projectConfig, pipelineFile, err := config.LoadProjectConfig(o.Dir)
	if err != nil {
		return err
	}

	isBootClone, err := existingBootClone(pipelineFile)
	if err != nil {
		return errors.Wrapf(err, "failed to check if this is an existing boot clone")
	}

	gitURL, gitRef, err := gits.GetGitInfoFromDirectory(o.Dir, o.Git())
	if err != nil {
		log.Logger().Warnf("there was a problem obtaining the boot config repository git configuration, falling back to defaults. Error: %s", err.Error())
		gitURL = config.DefaultBootRepository
		gitRef = config.DefaultVersionsRef
	}

	if o.GitURL != "" {
		log.Logger().Infof("GitURL provided, overriding the current value: %s", util.ColorInfo(gitURL))
		gitURL = o.GitURL
	}

	if o.GitRef != "" {
		log.Logger().Infof("GitRef provided, overriding the current value: %s", util.ColorInfo(gitRef))
		gitRef = o.GitRef
	}

	if config.LoadActiveInstallProfile() == config.CloudBeesProfile && o.GitURL == "" {
		gitURL = config.DefaultCloudBeesBootRepository
	}
	if config.LoadActiveInstallProfile() == config.CloudBeesProfile && o.VersionStreamURL == config.DefaultVersionsURL {
		o.VersionStreamURL = config.DefaultCloudBeesVersionsURL

	}
	if config.LoadActiveInstallProfile() == config.CloudBeesProfile && o.VersionStreamRef == config.DefaultVersionsRef {
		o.VersionStreamRef = config.DefaultCloudBeesVersionsRef

	}
	if gitURL == "" {
		return util.MissingOption("git-url")
	}

	requirements, requirementsFile, _ := config.LoadRequirementsConfig(o.Dir)

	// lets report errors parsing this file after the check we are outside of a git clone
	o.defaultVersionStream(requirements)

	if !isBootClone {
		log.Logger().Infof("No Jenkins X pipeline file %s or no jx boot requirements file %s found. You are not running this command from inside a "+
			"Jenkins X Boot git clone", info(pipelineFile), info(config.RequirementsConfigFileName))

		gitInfo, err := gits.ParseGitURL(gitURL)
		if err != nil {
			return errors.Wrapf(err, "failed to parse git URL %s", gitURL)
		}

		repo := gitInfo.Name
		cloneDir := filepath.Join(o.Dir, repo)

		if o.GitRef == "" {
			gitRef, err = o.determineGitRef(requirements, gitURL)
			if err != nil {
				return errors.Wrapf(err, "failed to determine git ref")
			}
		}

		if !o.BatchMode {
			log.Logger().Infof("To continue we will clone %s @ %s to %s", info(gitURL), info(gitRef), info(cloneDir))

			help := "A git clone of a Jenkins X Boot source repository is required for 'jx boot'"
			message := "Do you want to clone the Jenkins X Boot Git repository?"
			if !util.Confirm(message, true, help, o.In, o.Out, o.Err) {
				return fmt.Errorf("Please run this command again inside a git clone from a Jenkins X Boot repository")
			}
		}

		bootCloneExists, err := util.FileExists(cloneDir)
		if err != nil {
			return err
		}
		if bootCloneExists {
			return fmt.Errorf("Cannot clone git repository to %s as the dir already exists. Maybe try 'cd %s' and re-run the 'jx boot' command?", repo, repo)
		}

		log.Logger().Infof("Cloning %s @ %s to %s\n", info(gitURL), info(gitRef), info(cloneDir))

		err = os.MkdirAll(cloneDir, util.DefaultWritePermissions)
		if err != nil {
			return errors.Wrapf(err, "failed to create directory: %s", cloneDir)
		}

		err = o.Git().Clone(gitURL, cloneDir)
		if err != nil {
			return errors.Wrapf(err, "failed to clone git URL %s to directory: %s", gitURL, cloneDir)
		}
		commitish, err := gits.FindTagForVersion(cloneDir, gitRef, o.Git())
		if err != nil {
			log.Logger().Debugf(errors.Wrapf(err, "finding tag for %s", o.GitRef).Error())
			commitish = fmt.Sprintf("%s/%s", "origin", gitRef)
		}
		err = o.Git().Reset(cloneDir, commitish, true)
		if err != nil {
			return errors.Wrapf(err, "setting HEAD to %s", commitish)
		}
		o.Dir, err = filepath.Abs(cloneDir)
		if err != nil {
			return err
		}

		projectConfig, pipelineFile, err = config.LoadProjectConfig(o.Dir)
		if err != nil {
			return err
		}
		bootCloneExists, err = util.FileExists(pipelineFile)
		if err != nil {
			return err
		}

		if !bootCloneExists {
			return fmt.Errorf("The cloned repository %s does not include a Jenkins X Pipeline file at %s", gitURL, pipelineFile)
		}
	}

	requirements, requirementsFile, err = config.LoadRequirementsConfig(o.Dir)
	if err != nil {
		return errors.Wrap(err, "failed to load jx-requirements.yml file")
	}
	o.defaultVersionStream(requirements)

	if requirements.BootConfigURL == "" {
		requirements.BootConfigURL = gitURL
	}

	err = requirements.SaveConfig(requirementsFile)
	if err != nil {
		return err
	}

	isBootClone, err = util.FileExists(requirementsFile)
	if err != nil {
		return err
	}
	if !isBootClone {
		return fmt.Errorf("No requirements file %s are you sure you are running this command inside a GitOps clone?", requirementsFile)
	}

	err = o.verifyRequirements(requirements, requirementsFile)
	if err != nil {
		return err
	}

	log.Logger().Infof("booting up Jenkins X")

	// now lets really boot
	_, so := create.NewCmdStepCreateTaskAndOption(o.CommonOptions)
	so.CloneDir = o.Dir
	so.CloneDir = o.Dir
	so.InterpretMode = true
	so.NoReleasePrepare = true
	so.StartStep = o.StartStep
	so.EndStep = o.EndStep

	so.AdditionalEnvVars = map[string]string{
		"JX_NO_TILLER":    "true",
		"REPO_URL":        gitURL,
		"BASE_CONFIG_REF": gitRef,
	}
	if requirements.Cluster.HelmMajorVersion == "3" {
		so.AdditionalEnvVars["JX_HELM3"] = "true"
	} else {
		so.AdditionalEnvVars["JX_NO_TILLER"] = "true"
	}
	if o.HelmLogLevel != "" {
		so.AdditionalEnvVars["JX_HELM_VERBOSE"] = o.HelmLogLevel
	}

	// Set the namespace in the pipeline
	so.CommonOptions.SetDevNamespace(requirements.Cluster.Namespace)
	// lets ensure the namespace is set in the jenkins-x.yml file
	envVars := make([]v1.EnvVar, 0)
	for _, e := range projectConfig.PipelineConfig.Pipelines.Release.Pipeline.Environment {
		if e.Name == "DEPLOY_NAMESPACE" {
			envVars = append(envVars, v1.EnvVar{
				Name:  "DEPLOY_NAMESPACE",
				Value: requirements.Cluster.Namespace,
			})
		} else {
			envVars = append(envVars, e)
		}
	}
	projectConfig.PipelineConfig.Pipelines.Release.Pipeline.Environment = envVars
	err = projectConfig.SaveConfig(pipelineFile)
	if err != nil {
		return errors.Wrapf(err, "setting namespace in jenkins-x.yml")
	}
	so.VersionResolver, err = o.CreateVersionResolver(requirements.VersionStream.URL, requirements.VersionStream.Ref)
	if err != nil {
		return errors.Wrapf(err, "there was a problem creating a version resolver from versions stream repository %s and ref %s", requirements.VersionStream.URL, requirements.VersionStream.Ref)
	}

	if o.BatchMode {
		so.AdditionalEnvVars["JX_BATCH_MODE"] = "true"
	}
	err = so.Run()
	if err != nil {
		return errors.Wrapf(err, "failed to interpret pipeline file %s", pipelineFile)
	}

	// lets switch kubernetes context to it so the user can use `jx` commands immediately
	no := &namespace.NamespaceOptions{}
	no.CommonOptions = o.CommonOptions
	no.Args = []string{requirements.Cluster.Namespace}
	log.Logger().Infof("switching to the namespace %s so that you can use %s commands on the installation", info(requirements.Cluster.Namespace), info("jx"))
	return no.Run()
}

func existingBootClone(pipelineFile string) (bool, error) {
	pipelineExists, err := util.FileExists(pipelineFile)
	if err != nil {
		return false, err
	}
	requirementsExist, err := util.FileExists(config.RequirementsConfigFileName)
	if err != nil {
		return false, err
	}
	return requirementsExist && pipelineExists, nil
}

func (o *BootOptions) determineGitRef(requirements *config.RequirementsConfig, gitURL string) (string, error) {
	// If the GitRef is not overridden and is set to it's default value then look up the version number
	resolver, err := o.CreateVersionResolver(requirements.VersionStream.URL, requirements.VersionStream.Ref)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create version resolver")
	}
	gitRef, err := resolver.ResolveGitVersion(gitURL)
	if err != nil {
		return "", errors.Wrapf(err, fmt.Sprintf("failed to resolve version for %s in version stream %s",
			gitURL, requirements.VersionStream.URL))
	}
	if gitRef == "" {
		log.Logger().Infof("Attempting to resolve version for upstream boot config %s", util.ColorInfo(config.DefaultBootRepository))
		gitRef, err = resolver.ResolveGitVersion(config.DefaultBootRepository)
		if err != nil {
			return "", errors.Wrapf(err, fmt.Sprintf("failed to resolve version for %s in version stream %s",
				config.DefaultBootRepository, requirements.VersionStream.URL))
		}
	}
	return gitRef, nil
}

func (o *BootOptions) defaultVersionStream(requirements *config.RequirementsConfig) {
	if requirements.VersionStream.URL == "" && requirements.VersionStream.Ref == "" {
		requirements.VersionStream.URL = o.VersionStreamURL
		requirements.VersionStream.Ref = o.VersionStreamRef
	}
	// If we still don't have a complete version stream ref then we better set to a default
	if requirements.VersionStream.URL == "" || requirements.VersionStream.Ref == "" {
		log.Logger().Warnf("Incomplete version stream reference %s @ %s", requirements.VersionStream.URL, requirements.VersionStream.Ref)
		if config.LoadActiveInstallProfile() == config.CloudBeesProfile {
			o.VersionStreamRef = config.DefaultCloudBeesVersionsRef
			o.VersionStreamURL = config.DefaultVersionsURL
		} else {
			o.VersionStreamRef = config.DefaultVersionsRef
			o.VersionStreamURL = config.DefaultVersionsURL
		}
		log.Logger().Infof("Setting version stream reference to default %s @ %s", requirements.VersionStream.URL, requirements.VersionStream.Ref)
	}
}

func (o *BootOptions) verifyRequirements(requirements *config.RequirementsConfig, requirementsFile string) error {
	provider := requirements.Cluster.Provider
	if provider == "" {
		return config.MissingRequirement("provider", requirementsFile)
	}
	if provider == "" {
		if requirements.Cluster.ProjectID == "" {
			return config.MissingRequirement("project", requirementsFile)
		}
	}
	if requirements.Cluster.Namespace == "" {
		return config.MissingRequirement("namespace", requirementsFile)
	}
	return nil
}

func (o *BootOptions) verifyClusterConnection() error {
	client, err := o.KubeClient()
	if err == nil {
		_, err = client.CoreV1().Namespaces().List(metav1.ListOptions{})
	}

	if err != nil {
		return fmt.Errorf("You are not currently connected to a cluster, please connect to the cluster that you intend to %s\n"+
			"Alternatively create a new cluster using %s", util.ColorInfo("jx boot"), util.ColorInfo("jx create cluster"))
	}
	return nil
}

func (o *BootOptions) overrideSteps() {
	if o.StartStep == "" {
		startStep := os.Getenv("JX_BOOT_START_STEP")
		if startStep != "" {
			log.Logger().Debugf("Overriding start-step with env var: '%s'", startStep)
			o.StartStep = startStep
		}
	}

	if o.EndStep == "" {
		endStep := os.Getenv("JX_BOOT_END_STEP")
		if endStep != "" {
			log.Logger().Debugf("Overriding end-step with env var: '%s'", endStep)
			o.EndStep = endStep
		}
	}
}

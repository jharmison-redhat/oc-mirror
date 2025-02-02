package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"

	"github.com/RedHatGov/bundle/pkg/archive"
	"github.com/RedHatGov/bundle/pkg/bundle"
	"github.com/RedHatGov/bundle/pkg/config"
	"github.com/RedHatGov/bundle/pkg/config/v1alpha1"
	"github.com/RedHatGov/bundle/pkg/image"
	"github.com/RedHatGov/bundle/pkg/metadata"
	"github.com/RedHatGov/bundle/pkg/metadata/storage"
)

func (o *MirrorOptions) Create(ctx context.Context, flags *pflag.FlagSet) error {

	// Read the imageset-config.yaml
	cfg, err := config.LoadConfig(o.ConfigPath)
	if err != nil {
		return err
	}

	// Make sure the `opm` image exists during the publish step
	// since catalog images need to be rebuilt.
	cfg.Mirror.AdditionalImages = append(cfg.Mirror.AdditionalImages, v1alpha1.AdditionalImages{
		Image: v1alpha1.Image{Name: OPMImage},
	})

	logrus.Info("Verifying pull secrets")
	// Validating pull secrets
	if err := config.ValidateSecret(cfg); err != nil {
		return err
	}

	// Configure the metadata backend.
	backend, err := o.newBackendForConfig(ctx, cfg.StorageConfig)
	if err != nil {
		return fmt.Errorf("error opening backend: %v", err)
	}

	if err := bundle.MakeCreateDirs(o.Dir); err != nil {
		return err
	}

	// Run full or diff mirror.
	var meta v1alpha1.Metadata
	var thisRun v1alpha1.PastMirror
	switch err := backend.ReadMetadata(ctx, &meta, config.MetadataBasePath); {
	case err != nil && !errors.Is(err, storage.ErrMetadataNotExist):
		return err
	case err != nil && errors.Is(err, storage.ErrMetadataNotExist):
		thisRun, err = o.createFull(ctx, flags, cfg)
		if err != nil {
			return err
		}
		meta.Uid = uuid.New()
	case err == nil && len(meta.PastMirrors) != 0:
		thisRun, err = o.createDiff(ctx, flags, cfg, meta)
		if err != nil {
			return err
		}
	}

	// Update metadata files and get newly created filepaths.
	manifests, blobs, err := o.getFiles(meta)
	if err != nil {
		return err
	}
	// Store the config in the current run for reproducibility.
	thisRun.Mirror = cfg.Mirror
	// Add only the new manifests and blobs created to the current run.
	thisRun.Manifests = append(thisRun.Manifests, manifests...)
	thisRun.Blobs = append(thisRun.Blobs, blobs...)
	// Add this run and metadata to top level metadata.
	meta.PastMirrors = append(meta.PastMirrors, thisRun)
	meta.PastBlobs = append(meta.PastBlobs, blobs...)

	// Update the metadata.
	if err = metadata.UpdateMetadata(ctx, backend, &meta, o.SourceSkipTLS); err != nil {
		return err
	}

	// Run archiver
	if err := o.prepareArchive(cfg, thisRun.Sequence, manifests, blobs); err != nil {
		return err
	}

	// Handle Committer backends.
	if committer, isCommitter := backend.(storage.Committer); isCommitter {
		if err := committer.Commit(ctx); err != nil {
			return err
		}
	}

	return nil
}

// createFull performs all tasks in creating full imagesets
func (o *MirrorOptions) createFull(ctx context.Context, flags *pflag.FlagSet, cfg v1alpha1.ImageSetConfiguration) (run v1alpha1.PastMirror, err error) {
	run = v1alpha1.PastMirror{
		Sequence:  1,
		Timestamp: int(time.Now().Unix()),
	}

	allAssocs := image.AssociationSet{}

	if len(cfg.Mirror.OCP.Channels) != 0 {
		opts := NewReleaseOptions(*o, flags)
		assocs, err := opts.GetReleasesInitial(cfg)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if len(cfg.Mirror.Operators) != 0 {
		opts := NewOperatorOptions(*o)
		opts.SkipImagePin = o.SkipImagePin
		assocs, err := opts.Full(ctx, cfg)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if len(cfg.Mirror.Samples) != 0 {
		logrus.Debugf("sample images full not implemented")
	}

	if len(cfg.Mirror.AdditionalImages) != 0 {
		opts := NewAdditionalOptions(*o)
		assocs, err := opts.GetAdditional(cfg, cfg.Mirror.AdditionalImages)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if len(cfg.Mirror.Helm.Local) != 0 || len(cfg.Mirror.Helm.Repos) != 0 {
		opts := NewHelmOptions(*o)
		assocs, err := opts.PullCharts(cfg)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if err := o.writeAssociations(allAssocs); err != nil {
		return run, fmt.Errorf("error writing association file: %v", err)
	}

	return run, nil
}

// createDiff performs all tasks in creating differential imagesets
func (o *MirrorOptions) createDiff(ctx context.Context, flags *pflag.FlagSet, cfg v1alpha1.ImageSetConfiguration, meta v1alpha1.Metadata) (run v1alpha1.PastMirror, err error) {

	lastRun := meta.PastMirrors[len(meta.PastMirrors)-1]
	run = v1alpha1.PastMirror{
		Sequence:  lastRun.Sequence + 1,
		Timestamp: int(time.Now().Unix()),
	}

	allAssocs := image.AssociationSet{}

	if len(cfg.Mirror.OCP.Channels) != 0 {
		opts := NewReleaseOptions(*o, flags)
		assocs, err := opts.GetReleasesInitial(cfg)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if len(cfg.Mirror.Operators) != 0 {
		opts := NewOperatorOptions(*o)
		opts.SkipImagePin = o.SkipImagePin
		assocs, err := opts.Diff(ctx, cfg, lastRun)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if len(cfg.Mirror.Samples) != 0 {
		logrus.Debugf("sample images diff not implemented")
	}

	if len(cfg.Mirror.AdditionalImages) != 0 {
		opts := NewAdditionalOptions(*o)
		assocs, err := opts.GetAdditional(cfg, cfg.Mirror.AdditionalImages)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if len(cfg.Mirror.Helm.Local) != 0 || len(cfg.Mirror.Helm.Repos) != 0 {
		opts := NewHelmOptions(*o)
		assocs, err := opts.PullCharts(cfg)
		if err != nil {
			return run, err
		}
		allAssocs.Merge(assocs)
	}

	if err := o.writeAssociations(allAssocs); err != nil {
		return run, fmt.Errorf("error writing association file: %v", err)
	}

	return run, nil
}

// newBackendForConfig returns a Backend specified by config
func (o *MirrorOptions) newBackendForConfig(ctx context.Context, cfg v1alpha1.StorageConfig) (storage.Backend, error) {
	dir := filepath.Join(o.Dir, config.SourceDir)
	iface, err := storage.ByConfig(ctx, dir, cfg)
	if err != nil {
		return nil, err
	}

	b, ok := iface.(storage.Backend)
	if !ok {
		return nil, fmt.Errorf("error creating backend with provided config")
	}
	return b, err
}

func (o *MirrorOptions) prepareArchive(cfg v1alpha1.ImageSetConfiguration, seq int, manifests []v1alpha1.Manifest, blobs []v1alpha1.Blob) error {

	// Default to a 500GiB archive size.
	var segSize int64 = 500
	if cfg.ImageSetConfigurationSpec.ArchiveSize != 0 {
		segSize = cfg.ImageSetConfigurationSpec.ArchiveSize
		logrus.Debugf("Using user provider archive size %d GiB", segSize)
	}
	segSize *= 1024 * 1024 * 1024

	cwd, err := os.Getwd()

	if err != nil {
		return err
	}

	// Set get absolute path to output dir
	output, err := filepath.Abs(o.OutputDir)

	if err != nil {
		return err
	}

	// Change dir before archiving to avoid issues with symlink paths
	if err := os.Chdir(filepath.Join(o.Dir, config.SourceDir)); err != nil {
		return err
	}
	defer os.Chdir(cwd)

	packager := archive.NewPackager(manifests, blobs)
	prefix := fmt.Sprintf("mirror_seq%d", seq)

	// Create tar archive
	if err := packager.CreateSplitArchive(segSize, output, ".", prefix, o.SkipCleanup); err != nil {
		return fmt.Errorf("failed to create archive: %v", err)
	}

	return nil

}

func (o *MirrorOptions) getFiles(meta v1alpha1.Metadata) ([]v1alpha1.Manifest, []v1alpha1.Blob, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}

	// Change dir before archiving to avoid issues with symlink paths
	if err := os.Chdir(filepath.Join(o.Dir, config.SourceDir)); err != nil {
		return nil, nil, err
	}
	defer os.Chdir(cwd)

	// Gather manifests we pulled
	manifests, err := bundle.ReconcileManifests()

	if err != nil {
		return nil, nil, err
	}

	blobs, err := bundle.ReconcileBlobs(meta)

	if err != nil {
		return nil, nil, err
	}

	return manifests, blobs, nil
}

func (o *MirrorOptions) writeAssociations(assocs image.AssociationSet) error {
	assocPath := filepath.Join(o.Dir, config.SourceDir, config.AssociationsBasePath)
	if err := os.MkdirAll(filepath.Dir(assocPath), 0755); err != nil {
		return fmt.Errorf("mkdir image associations file: %v", err)
	}
	f, err := os.OpenFile(assocPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("open image associations file: %v", err)
	}
	defer f.Close()
	return assocs.Encode(f)
}

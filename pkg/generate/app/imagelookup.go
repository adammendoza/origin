package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fsouza/go-dockerclient"

	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/errors"
	utilerrors "github.com/GoogleCloudPlatform/kubernetes/pkg/util/errors"
	"github.com/golang/glog"

	"github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/dockerregistry"
	imageapi "github.com/openshift/origin/pkg/image/api"
)

type DockerClientResolver struct {
	Client *docker.Client

	// Optional, will delegate resolution to the registry if no local
	// exact matches are found.
	RegistryResolver Resolver
}

func (r DockerClientResolver) Resolve(value string) (*ComponentMatch, error) {
	registry, namespace, name, tag, err := imageapi.SplitDockerPullSpec(value)
	if err != nil {
		return nil, err
	}

	glog.V(4).Infof("checking local Docker daemon %s/%s/%s with tag %q", registry, namespace, name, tag)
	images, err := r.Client.ListImages(docker.ListImagesOptions{})
	if err != nil {
		return nil, err
	}
	matches := ScoredComponentMatches{}
	for _, image := range images {
		if tags := matchTag(image, value, registry, namespace, name, tag); len(tags) > 0 {
			matches = append(matches, tags...)
		}
	}
	sort.Sort(matches)
	if exact := matches.Exact(); len(exact) > 0 {
		matches = exact
	} else {
		if r.RegistryResolver != nil {
			match, err := r.RegistryResolver.Resolve(value)
			switch err.(type) {
			case nil:
				return match, nil
			case ErrNoMatch:
				// show our partial matches
			case ErrMultipleMatches:
				// TODO: add these matches to our local results
				return nil, err
			default:
				return nil, err
			}
		}
	}

	errs := []error{}
	for i, match := range matches {
		if match.Image != nil {
			continue
		}
		updated, err := r.lookup(match.Value)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		updated.Score = match.Score
		updated.ImageTag = tag
		matches[i] = updated
	}

	if len(errs) != 0 {
		if len(errs) == 1 {
			err := errs[0]
			if err == docker.ErrNoSuchImage {
				return nil, ErrNoMatch{value: value}
			}
			return nil, err
		}
		return nil, utilerrors.NewAggregate(errs)
	}

	switch len(matches) {
	case 0:
		return nil, ErrNoMatch{value: value}
	case 1:
		return matches[0], nil
	default:
		return nil, ErrMultipleMatches{Image: value, Matches: matches}
	}
}

func (r DockerClientResolver) lookup(value string) (*ComponentMatch, error) {
	image, err := r.Client.InspectImage(value)
	if err != nil {
		return nil, err
	}
	dockerImage := &imageapi.DockerImage{}
	if err := kapi.Scheme.Convert(image, dockerImage); err != nil {
		return nil, err
	}
	return &ComponentMatch{
		Value:       value,
		Argument:    fmt.Sprintf("--docker-image=%q", value),
		Name:        value,
		Description: descriptionFor(dockerImage, value, "local Docker"),
		Builder:     IsBuilderImage(dockerImage),
		Score:       0.0,
		Image:       dockerImage,
	}, nil
}

type DockerRegistryResolver struct {
	Client dockerregistry.Client
}

func (r DockerRegistryResolver) Resolve(value string) (*ComponentMatch, error) {
	registry, namespace, name, tag, err := imageapi.SplitDockerPullSpec(value)
	if err != nil {
		return nil, err
	}
	glog.V(4).Infof("checking Docker registry %s/%s/%s with tag %q", registry, namespace, name, tag)
	connection, err := r.Client.Connect(registry)
	if err != nil {
		if dockerregistry.IsRegistryNotFound(err) {
			return nil, ErrNoMatch{value: value}
		}
		return nil, ErrNoMatch{value: value, qualifier: fmt.Sprintf("can't connect to %q: %v", registry, err)}
	}
	image, err := connection.ImageByTag(namespace, name, tag)
	if err != nil {
		if dockerregistry.IsNotFound(err) {
			return nil, ErrNoMatch{value: value, qualifier: err.Error()}
		}
		return nil, ErrNoMatch{value: value, qualifier: fmt.Sprintf("can't connect to %q: %v", registry, err)}
	}
	if len(tag) == 0 {
		tag = "latest"
	}
	glog.V(4).Infof("found image: %#v", image)
	dockerImage := &imageapi.DockerImage{}
	if err = kapi.Scheme.Convert(image, dockerImage); err != nil {
		return nil, err
	}

	from := registry
	if len(registry) == 0 {
		registry = "Docker Hub"
	}
	return &ComponentMatch{
		Value:       value,
		Argument:    fmt.Sprintf("--docker-image=%q", value),
		Name:        value,
		Description: descriptionFor(dockerImage, value, from),
		Builder:     IsBuilderImage(dockerImage),
		Score:       0,
		Image:       dockerImage,
		ImageTag:    tag,
	}, nil
}

func descriptionFor(image *imageapi.DockerImage, value, from string) string {
	shortID := image.ID
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}
	parts := []string{fmt.Sprintf("Docker image %q", value), shortID, fmt.Sprintf("from %s", from)}
	if image.Size > 0 {
		mb := float64(image.Size) / float64(1024*1024)
		parts = append(parts, fmt.Sprintf("%f", mb))
	}
	if len(image.Author) > 0 {
		parts = append(parts, fmt.Sprintf("author %s", image.Author))
	}
	if len(image.Comment) > 0 {
		parts = append(parts, image.Comment)
	}
	return strings.Join(parts, ", ")
}

func partialScorer(a, b string, prefix bool, partial, none float32) (bool, float32) {
	switch {
	case len(a) == 0 && len(b) != 0, len(a) != 0 && len(b) == 0:
		return true, partial
	case a != b:
		if prefix {
			if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
				return true, partial
			}
		}
		return false, none
	default:
		return true, 0.0
	}
}

func matchTag(image docker.APIImages, value, registry, namespace, name, tag string) []*ComponentMatch {
	if len(tag) == 0 {
		tag = "latest"
	}
	matches := []*ComponentMatch{}
	for _, s := range image.RepoTags {
		if value == s {
			matches = append(matches, &ComponentMatch{
				Value: s,
				Score: 0.0,
			})
			continue
		}
		iRegistry, iNamespace, iName, iTag, err := imageapi.SplitDockerPullSpec(s)
		if err != nil {
			continue
		}
		if len(iTag) == 0 {
			iTag = "latest"
		}
		match := &ComponentMatch{}
		ok, score := partialScorer(name, iName, true, 0.5, 1.0)
		if !ok {
			continue
		}
		match.Score += score
		_, score = partialScorer(namespace, iNamespace, false, 0.5, 1.0)
		match.Score += score
		_, score = partialScorer(registry, iRegistry, false, 0.5, 1.0)
		match.Score += score
		_, score = partialScorer(tag, iTag, false, 0.5, 1.0)
		match.Score += score

		if match.Score >= 4.0 {
			continue
		}
		match.Score = match.Score / 4.0
		glog.V(4).Infof("partial match on %q with %f", s, match.Score)
		match.Value = s
		matches = append(matches, match)
	}
	return matches
}

type ImageStreamResolver struct {
	Client     client.ImageRepositoriesNamespacer
	Images     client.ImagesNamespacer
	Namespaces []string
}

func (r ImageStreamResolver) Resolve(value string) (*ComponentMatch, error) {
	registry, namespace, name, tag, err := imageapi.SplitOpenShiftPullSpec(value)
	if err != nil || len(registry) != 0 {
		return nil, fmt.Errorf("image repositories must be of the form [<namespace>/]<name>[:<tag>]")
	}
	namespaces := r.Namespaces
	if len(namespace) != 0 {
		namespaces = []string{namespace}
	}
	for _, namespace := range namespaces {
		glog.V(4).Infof("checking image stream %s/%s with tag %q", namespace, name, tag)
		repo, err := r.Client.ImageRepositories(namespace).Get(name)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		searchTag := tag
		// TODO: move to a lookup function on repo, or better yet, have the repo.Status.Tags field automatically infer latest
		if len(searchTag) == 0 {
			searchTag = "latest"
		}
		id, ok := repo.Tags[searchTag]
		if !ok {
			if len(tag) == 0 {
				return nil, ErrNoMatch{value: value, qualifier: fmt.Sprintf("the default tag %q has not been set", searchTag)}
			}
			return nil, ErrNoMatch{value: value, qualifier: fmt.Sprintf("tag %q has not been set", tag)}
		}
		imageData, err := r.Images.Images(namespace).Get(id)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, ErrNoMatch{value: value, qualifier: fmt.Sprintf("tag %q is set, but image %q has been removed", tag, id)}
			}
			return nil, err
		}

		spec := imageapi.JoinDockerPullSpec("", namespace, name, tag)
		return &ComponentMatch{
			Value:       spec,
			Argument:    fmt.Sprintf("--image=%q", spec),
			Name:        name,
			Description: fmt.Sprintf("Image stream %s (tag %q) in namespace %s, tracks %q", name, searchTag, namespace, repo.Status.DockerImageRepository),
			Builder:     IsBuilderImage(&imageData.DockerImageMetadata),
			Score:       0,

			ImageStream: repo,
			Image:       &imageData.DockerImageMetadata,
			ImageTag:    searchTag,
		}, nil
	}
	return nil, ErrNoMatch{value: value}
}

type Searcher interface {
	Search(terms []string) ([]*ComponentMatch, error)
}

func InputImageFromMatch(match *ComponentMatch) (*ImageRef, error) {
	switch {
	case match.ImageStream != nil:
		input, err := ImageFromRepository(match.ImageStream, match.ImageTag)
		if err != nil {
			return nil, err
		}
		input.AsImageRepository = true
		input.Info = match.Image
		return input, nil

	case match.Image != nil:
		input, err := ImageFromName(match.Value, match.ImageTag)
		if err != nil {
			return nil, err
		}
		input.AsImageRepository = false
		input.Info = match.Image
		return input, nil

	default:
		return nil, fmt.Errorf("no image or image stream, can't setup a build")
	}
}

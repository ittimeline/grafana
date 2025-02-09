package folders

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	"github.com/grafana/grafana/pkg/apis/folders/v0alpha1"
	"github.com/grafana/grafana/pkg/infra/appcontext"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/grafana-apiserver/endpoints/request"
	"github.com/grafana/grafana/pkg/services/grafana-apiserver/storage/entity"
	"github.com/grafana/grafana/pkg/services/grafana-apiserver/utils"
	"github.com/grafana/grafana/pkg/util"
)

var (
	_ rest.Scoper               = (*legacyStorage)(nil)
	_ rest.SingularNameProvider = (*legacyStorage)(nil)
	_ rest.Getter               = (*legacyStorage)(nil)
	_ rest.Lister               = (*legacyStorage)(nil)
	_ rest.Storage              = (*legacyStorage)(nil)
	_ rest.Creater              = (*legacyStorage)(nil)
	_ rest.Updater              = (*legacyStorage)(nil)
	_ rest.GracefulDeleter      = (*legacyStorage)(nil)
)

type legacyStorage struct {
	service        folder.Service
	namespacer     request.NamespaceMapper
	tableConverter rest.TableConvertor
}

func (s *legacyStorage) New() runtime.Object {
	return resourceInfo.NewFunc()
}

func (s *legacyStorage) Destroy() {}

func (s *legacyStorage) NamespaceScoped() bool {
	return true // namespace == org
}

func (s *legacyStorage) GetSingularName() string {
	return resourceInfo.GetSingularName()
}

func (s *legacyStorage) NewList() runtime.Object {
	return resourceInfo.NewListFunc()
}

func (s *legacyStorage) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	return s.tableConverter.ConvertToTable(ctx, object, tableOptions)
}

func (s *legacyStorage) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	orgId, err := request.OrgIDForList(ctx)
	if err != nil {
		return nil, err
	}

	parentUID := ""
	fieldRequirements, fieldSelector, err := entity.ReadFieldRequirements(options.FieldSelector)
	if err != nil {
		return nil, err
	}
	if fieldRequirements.Folder != nil {
		parentUID = *fieldRequirements.Folder
	}
	// Update the field selector to remove the unneeded selectors
	options.FieldSelector = fieldSelector

	paging, err := readContinueToken(options)
	if err != nil {
		return nil, err
	}

	user, err := appcontext.User(ctx)
	if err != nil {
		return nil, err
	}

	// When nested folders are not enabled, all folders are root folders
	hits, err := s.service.GetChildren(ctx, &folder.GetChildrenQuery{
		UID:          parentUID, // NOTE!  we should do a different query when nested folders are enabled!
		SignedInUser: user,
		Limit:        paging.page,
		OrgID:        orgId,
		Page:         paging.limit,
	})
	if err != nil {
		return nil, err
	}

	list := &v0alpha1.FolderList{}
	for _, v := range hits {
		list.Items = append(list.Items, *convertToK8sResource(v, s.namespacer))
	}
	if len(list.Items) >= int(paging.limit) {
		list.Continue = paging.GetNextPageToken()
	}
	return list, nil
}

func (s *legacyStorage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	info, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, err
	}

	user, err := appcontext.User(ctx)
	if err != nil {
		return nil, err
	}

	dto, err := s.service.Get(ctx, &folder.GetFolderQuery{
		SignedInUser: user,
		UID:          &name,
		OrgID:        info.OrgID,
	})
	if err != nil || dto == nil {
		if errors.Is(err, dashboards.ErrFolderNotFound) || err == nil {
			err = resourceInfo.NewNotFound(name)
		}
		return nil, err
	}

	return convertToK8sResource(dto, s.namespacer), nil
}

func (s *legacyStorage) Create(ctx context.Context,
	obj runtime.Object,
	createValidation rest.ValidateObjectFunc,
	options *metav1.CreateOptions,
) (runtime.Object, error) {
	info, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, err
	}

	user, err := appcontext.User(ctx)
	if err != nil {
		return nil, err
	}

	p, ok := obj.(*v0alpha1.Folder)
	if !ok {
		return nil, fmt.Errorf("expected folder?")
	}

	// Simplify creating unique folder names with
	if p.GenerateName != "" && strings.Contains(p.Spec.Title, "${RAND}") {
		rand, _ := util.GetRandomString(10)
		p.Spec.Title = strings.ReplaceAll(p.Spec.Title, "${RAND}", rand)
	}

	accessor, err := utils.MetaAccessor(p)
	if err != nil {
		return nil, err
	}
	parent := accessor.GetFolder()

	out, err := s.service.Create(ctx, &folder.CreateFolderCommand{
		SignedInUser: user,
		UID:          p.Name,
		Title:        p.Spec.Title,
		Description:  p.Spec.Description,
		OrgID:        info.OrgID,
		ParentUID:    parent,
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, out.UID, nil)
}

func (s *legacyStorage) Update(ctx context.Context,
	name string,
	objInfo rest.UpdatedObjectInfo,
	createValidation rest.ValidateObjectFunc,
	updateValidation rest.ValidateObjectUpdateFunc,
	forceAllowCreate bool,
	options *metav1.UpdateOptions,
) (runtime.Object, bool, error) {
	info, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, false, err
	}

	user, err := appcontext.User(ctx)
	if err != nil {
		return nil, false, err
	}

	created := false
	oldObj, err := s.Get(ctx, name, nil)
	if err != nil {
		return oldObj, created, err
	}

	obj, err := objInfo.UpdatedObject(ctx, oldObj)
	if err != nil {
		return oldObj, created, err
	}
	f, ok := obj.(*v0alpha1.Folder)
	if !ok {
		return nil, created, fmt.Errorf("expected folder after update")
	}
	old, ok := oldObj.(*v0alpha1.Folder)
	if !ok {
		return nil, created, fmt.Errorf("expected old object to be a folder also")
	}

	mOld, _ := utils.MetaAccessor(old)
	mNew, _ := utils.MetaAccessor(f)
	oldParent := mOld.GetFolder()
	newParent := mNew.GetFolder()
	if oldParent != newParent {
		_, err = s.service.Move(ctx, &folder.MoveFolderCommand{
			SignedInUser: user,
			UID:          name,
			OrgID:        info.OrgID,
			NewParentUID: newParent,
		})
		if err != nil {
			return nil, created, fmt.Errorf("error changing parent folder spec")
		}
	}

	changed := false
	cmd := &folder.UpdateFolderCommand{
		SignedInUser: user,
		UID:          name,
		OrgID:        info.OrgID,
		Overwrite:    true,
	}
	if f.Spec.Title != old.Spec.Title {
		cmd.NewTitle = &f.Spec.Title
		changed = true
	}
	if f.Spec.Description != old.Spec.Description {
		cmd.NewDescription = &f.Spec.Description
		changed = true
	}
	if changed {
		_, err = s.service.Update(ctx, cmd)
		if err != nil {
			return nil, false, err
		}
	}

	r, err := s.Get(ctx, name, nil)
	return r, created, err
}

// GracefulDeleter
func (s *legacyStorage) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	v, err := s.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return v, false, err // includes the not-found error
	}
	user, err := appcontext.User(ctx)
	if err != nil {
		return nil, false, err
	}
	info, err := request.NamespaceInfoFrom(ctx, true)
	if err != nil {
		return nil, false, err
	}
	p, ok := v.(*v0alpha1.Folder)
	if !ok {
		return v, false, fmt.Errorf("expected a folder response from Get")
	}
	err = s.service.Delete(ctx, &folder.DeleteFolderCommand{
		UID:          name,
		OrgID:        info.OrgID,
		SignedInUser: user,

		// This would cascade delete into alert rules
		ForceDeleteRules: false,
	})
	return p, true, err // true is instant delete
}

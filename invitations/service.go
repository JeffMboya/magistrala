// Copyright (c) Abstract Machines
// SPDX-License-Identifier: Apache-2.0

package invitations

import (
	"context"
	"time"

	"github.com/absmach/magistrala"
	"github.com/absmach/magistrala/auth"
	grpcclient "github.com/absmach/magistrala/auth/api/grpc"
	"github.com/absmach/magistrala/pkg/errors"
	svcerr "github.com/absmach/magistrala/pkg/errors/service"
	mgsdk "github.com/absmach/magistrala/pkg/sdk/go"
)

type service struct {
	repo Repository
	auth grpcclient.AuthServiceClient
	sdk  mgsdk.SDK
}

// ErrMemberExist indicates that the user is already a member of the domain.
var ErrMemberExist = errors.New("user is already a member of the domain")

func NewService(repo Repository, authClient grpcclient.AuthServiceClient, sdk mgsdk.SDK) Service {
	return &service{
		repo: repo,
		auth: authClient,
		sdk:  sdk,
	}
}

func (svc *service) SendInvitation(ctx context.Context, token string, invitation Invitation) error {
	if err := CheckRelation(invitation.Relation); err != nil {
		return err
	}

	user, err := svc.identify(ctx, token)
	if err != nil {
		return err
	}
	invitation.InvitedBy = user.GetUserId()

	domainUserId := auth.EncodeDomainUserID(invitation.DomainID, invitation.UserID)
	if err := svc.authorize(ctx, domainUserId, auth.MembershipPermission, auth.DomainType, invitation.DomainID); err == nil {
		// return error if the user is already a member of the domain
		return errors.Wrap(svcerr.ErrConflict, ErrMemberExist)
	}

	if err := svc.checkAdmin(ctx, user.GetId(), invitation.DomainID); err != nil {
		return err
	}

	joinToken, err := svc.auth.Issue(ctx, &magistrala.IssueReq{UserId: user.GetUserId(), DomainId: &invitation.DomainID, Type: uint32(auth.InvitationKey)})
	if err != nil {
		return err
	}
	invitation.Token = joinToken.GetAccessToken()

	if invitation.Resend {
		invitation.UpdatedAt = time.Now()

		return svc.repo.UpdateToken(ctx, invitation)
	}

	invitation.CreatedAt = time.Now()

	return svc.repo.Create(ctx, invitation)
}

func (svc *service) ViewInvitation(ctx context.Context, token, userID, domainID string) (invitation Invitation, err error) {
	user, err := svc.identify(ctx, token)
	if err != nil {
		return Invitation{}, err
	}
	inv, err := svc.repo.Retrieve(ctx, userID, domainID)
	if err != nil {
		return Invitation{}, err
	}
	inv.Token = ""

	if user.GetUserId() == userID {
		return inv, nil
	}

	if inv.InvitedBy == user.GetUserId() {
		return inv, nil
	}

	if err := svc.checkAdmin(ctx, user.GetId(), domainID); err != nil {
		return Invitation{}, err
	}

	return inv, nil
}

func (svc *service) ListInvitations(ctx context.Context, token string, page Page) (invitations InvitationPage, err error) {
	user, err := svc.identify(ctx, token)
	if err != nil {
		return InvitationPage{}, err
	}

	if err := svc.authorize(ctx, user.GetId(), auth.AdminPermission, auth.PlatformType, auth.MagistralaObject); err == nil {
		return svc.repo.RetrieveAll(ctx, page)
	}

	if page.DomainID != "" {
		if err := svc.checkAdmin(ctx, user.GetId(), page.DomainID); err != nil {
			return InvitationPage{}, err
		}

		return svc.repo.RetrieveAll(ctx, page)
	}

	page.InvitedByOrUserID = user.GetUserId()

	return svc.repo.RetrieveAll(ctx, page)
}

func (svc *service) AcceptInvitation(ctx context.Context, token, domainID string) error {
	user, err := svc.identify(ctx, token)
	if err != nil {
		return err
	}

	inv, err := svc.repo.Retrieve(ctx, user.GetUserId(), domainID)
	if err != nil {
		return err
	}

	if inv.UserID == user.GetUserId() && inv.ConfirmedAt.IsZero() {
		req := mgsdk.UsersRelationRequest{
			Relation: inv.Relation,
			UserIDs:  []string{user.GetUserId()},
		}
		if sdkerr := svc.sdk.AddUserToDomain(inv.DomainID, req, inv.Token); sdkerr != nil {
			return sdkerr
		}

		inv.ConfirmedAt = time.Now()
		inv.UpdatedAt = time.Now()
		if err := svc.repo.UpdateConfirmation(ctx, inv); err != nil {
			return err
		}
	}

	return nil
}

func (svc *service) DeleteInvitation(ctx context.Context, token, userID, domainID string) error {
	user, err := svc.identify(ctx, token)
	if err != nil {
		return err
	}
	if user.GetUserId() == userID {
		return svc.repo.Delete(ctx, userID, domainID)
	}

	inv, err := svc.repo.Retrieve(ctx, userID, domainID)
	if err != nil {
		return err
	}

	if inv.InvitedBy == user.GetUserId() {
		return svc.repo.Delete(ctx, userID, domainID)
	}

	if err := svc.checkAdmin(ctx, user.GetId(), domainID); err != nil {
		return err
	}

	return svc.repo.Delete(ctx, userID, domainID)
}

func (svc *service) identify(ctx context.Context, token string) (*magistrala.IdentityRes, error) {
	user, err := svc.auth.Identify(ctx, &magistrala.IdentityReq{Token: token})
	if err != nil {
		return &magistrala.IdentityRes{}, err
	}

	return user, nil
}

func (svc *service) authorize(ctx context.Context, subj, perm, objType, obj string) error {
	req := &magistrala.AuthorizeReq{
		SubjectType: auth.UserType,
		SubjectKind: auth.UsersKind,
		Subject:     subj,
		Permission:  perm,
		ObjectType:  objType,
		Object:      obj,
	}
	res, err := svc.auth.Authorize(ctx, req)
	if err != nil {
		return errors.Wrap(svcerr.ErrAuthorization, err)
	}

	if !res.GetAuthorized() {
		return errors.Wrap(svcerr.ErrAuthorization, err)
	}

	return nil
}

// checkAdmin checks if the given user is a domain or platform administrator.
func (svc *service) checkAdmin(ctx context.Context, userID, domainID string) error {
	if err := svc.authorize(ctx, userID, auth.AdminPermission, auth.DomainType, domainID); err == nil {
		return nil
	}

	if err := svc.authorize(ctx, userID, auth.AdminPermission, auth.PlatformType, auth.MagistralaObject); err == nil {
		return nil
	}

	return svcerr.ErrAuthorization
}

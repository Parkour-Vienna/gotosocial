/*
   GoToSocial
   Copyright (C) 2021 GoToSocial Authors admin@gotosocial.org

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package db

import "github.com/superseriousbusiness/gotosocial/internal/gtsmodel"

// Relationship contains functions for getting or modifying the relationship between two accounts.
type Relationship interface {
	// IsBlocked checks whether account 1 has a block in place against block2.
	// If eitherDirection is true, then the function returns true if account1 blocks account2, OR if account2 blocks account1.
	IsBlocked(account1 string, account2 string, eitherDirection bool) (bool, Error)

	// GetBlock returns the block from account1 targeting account2, if it exists, or an error if it doesn't.
	//
	// Because this is slower than Blocked, only use it if you need the actual Block struct for some reason,
	// not if you're just checking for the existence of a block.
	GetBlock(account1 string, account2 string) (*gtsmodel.Block, Error)

	// GetRelationship retrieves the relationship of the targetAccount to the requestingAccount.
	GetRelationship(requestingAccount string, targetAccount string) (*gtsmodel.Relationship, Error)

	// IsFollowing returns true if sourceAccount follows target account, or an error if something goes wrong while finding out.
	IsFollowing(sourceAccount *gtsmodel.Account, targetAccount *gtsmodel.Account) (bool, Error)

	// IsFollowRequested returns true if sourceAccount has requested to follow target account, or an error if something goes wrong while finding out.
	IsFollowRequested(sourceAccount *gtsmodel.Account, targetAccount *gtsmodel.Account) (bool, Error)

	// IsMutualFollowing returns true if account1 and account2 both follow each other, or an error if something goes wrong while finding out.
	IsMutualFollowing(account1 *gtsmodel.Account, account2 *gtsmodel.Account) (bool, Error)

	// AcceptFollowRequest moves a follow request in the database from the follow_requests table to the follows table.
	// In other words, it should create the follow, and delete the existing follow request.
	//
	// It will return the newly created follow for further processing.
	AcceptFollowRequest(originAccountID string, targetAccountID string) (*gtsmodel.Follow, Error)

	// GetAccountFollowRequests returns all follow requests targeting the given account.
	GetAccountFollowRequests(accountID string) ([]*gtsmodel.FollowRequest, Error)

	// GetAccountFollows returns a slice of follows owned by the given accountID.
	GetAccountFollows(accountID string) ([]*gtsmodel.Follow, Error)

	// CountAccountFollows returns the amount of accounts that the given accountID is following.
	//
	// If localOnly is set to true, then only follows from *this instance* will be returned.
	CountAccountFollows(accountID string, localOnly bool) (int, Error)

	// GetAccountFollowedBy fetches follows that target given accountID.
	//
	// If localOnly is set to true, then only follows from *this instance* will be returned.
	GetAccountFollowedBy(accountID string, localOnly bool) ([]*gtsmodel.Follow, Error)

	// CountAccountFollowedBy returns the amounts that the given ID is followed by.
	CountAccountFollowedBy(accountID string, localOnly bool) (int, Error)
}
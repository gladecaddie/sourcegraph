package graphqlbackend

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	gqlerrors "github.com/graph-gophers/graphql-go/errors"
	"github.com/stretchr/testify/assert"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/envvar"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/auth"
	"github.com/sourcegraph/sourcegraph/internal/conf"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/dbmocks"
	"github.com/sourcegraph/sourcegraph/internal/gitserver"
	"github.com/sourcegraph/sourcegraph/internal/gqlutil"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/errors"
	"github.com/sourcegraph/sourcegraph/schema"
)

func TestUser(t *testing.T) {
	db := dbmocks.NewMockDB()
	t.Run("by username", func(t *testing.T) {
		checkUserByUsername := func(t *testing.T) {
			t.Helper()
			RunTests(t, []*Test{
				{
					Schema: mustParseGraphQLSchema(t, db),
					Query: `
				{
					user(username: "alice") {
						username
					}
				}
			`,
					ExpectedResult: `
				{
					"user": {
						"username": "alice"
					}
				}
			`,
				},
			})
		}

		users := dbmocks.NewMockUserStore()
		users.GetByUsernameFunc.SetDefaultHook(func(ctx context.Context, username string) (*types.User, error) {
			assert.Equal(t, "alice", username)
			return &types.User{ID: 1, Username: "alice"}, nil
		})
		db.UsersFunc.SetDefaultReturn(users)

		t.Run("allowed on Sourcegraph.com", func(t *testing.T) {
			orig := envvar.SourcegraphDotComMode()
			envvar.MockSourcegraphDotComMode(true)
			defer envvar.MockSourcegraphDotComMode(orig)

			checkUserByUsername(t)
		})

		t.Run("allowed on non-Sourcegraph.com", func(t *testing.T) {
			checkUserByUsername(t)
		})
	})

	t.Run("by email", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByVerifiedEmailFunc.SetDefaultHook(func(ctx context.Context, email string) (*types.User, error) {
			assert.Equal(t, "alice@example.com", email)
			return &types.User{ID: 1, Username: "alice"}, nil
		})
		db.UsersFunc.SetDefaultReturn(users)

		t.Run("disallowed on Sourcegraph.com", func(t *testing.T) {
			checkUserByEmailError := func(t *testing.T, wantErr string) {
				t.Helper()
				RunTests(t, []*Test{
					{
						Schema: mustParseGraphQLSchema(t, db),
						Query: `
				{
					user(email: "alice@example.com") {
						username
					}
				}
			`,
						ExpectedResult: `{"user": null}`,
						ExpectedErrors: []*gqlerrors.QueryError{
							{
								Path:          []any{"user"},
								Message:       wantErr,
								ResolverError: errors.New(wantErr),
							},
						},
					},
				})
			}

			orig := envvar.SourcegraphDotComMode()
			envvar.MockSourcegraphDotComMode(true)
			defer envvar.MockSourcegraphDotComMode(orig)

			t.Run("for anonymous viewer", func(t *testing.T) {
				users.GetByCurrentAuthUserFunc.SetDefaultReturn(nil, database.ErrNoCurrentUser)
				checkUserByEmailError(t, "not authenticated")
			})
			t.Run("for non-site-admin viewer", func(t *testing.T) {
				users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{SiteAdmin: false}, nil)
				checkUserByEmailError(t, "must be site admin")
			})
		})

		t.Run("allowed on non-Sourcegraph.com", func(t *testing.T) {
			RunTests(t, []*Test{
				{
					Schema: mustParseGraphQLSchema(t, db),
					Query: `
				{
					user(email: "alice@example.com") {
						username
					}
				}
			`,
					ExpectedResult: `
				{
					"user": {
						"username": "alice"
					}
				}
			`,
				},
			})
		})
	})
}

func TestUser_Email(t *testing.T) {
	db := dbmocks.NewMockDB()
	user := &types.User{ID: 1}
	ctx := actor.WithActor(context.Background(), actor.FromActualUser(user))

	t.Run("allowed by authenticated site admin user", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: 2, SiteAdmin: true}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		userEmails := dbmocks.NewMockUserEmailsStore()
		userEmails.GetPrimaryEmailFunc.SetDefaultReturn("john@doe.com", true, nil)
		db.UserEmailsFunc.SetDefaultReturn(userEmails)

		email, _ := NewUserResolver(ctx, db, user).Email(actor.WithActor(context.Background(), &actor.Actor{UID: 2}))
		got := fmt.Sprintf("%v", email)
		want := "john@doe.com"
		assert.Equal(t, want, got)
	})

	t.Run("allowed by authenticated user", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(user, nil)
		db.UsersFunc.SetDefaultReturn(users)

		userEmails := dbmocks.NewMockUserEmailsStore()
		userEmails.GetPrimaryEmailFunc.SetDefaultReturn("john@doe.com", true, nil)
		db.UserEmailsFunc.SetDefaultReturn(userEmails)

		email, _ := NewUserResolver(ctx, db, user).Email(actor.WithActor(context.Background(), &actor.Actor{UID: 1}))
		got := fmt.Sprintf("%v", email)
		want := "john@doe.com"
		assert.Equal(t, want, got)
	})
}

func TestUser_LatestSettings(t *testing.T) {
	db := dbmocks.NewMockDB()
	t.Run("only allowed by authenticated user on Sourcegraph.com", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		db.UsersFunc.SetDefaultReturn(users)

		orig := envvar.SourcegraphDotComMode()
		envvar.MockSourcegraphDotComMode(true)
		defer envvar.MockSourcegraphDotComMode(orig)

		tests := []struct {
			name  string
			ctx   context.Context
			setup func()
		}{
			{
				name: "unauthenticated",
				ctx:  context.Background(),
				setup: func() {
					users.GetByIDFunc.SetDefaultReturn(&types.User{ID: 1}, nil)
				},
			},
			{
				name: "another user",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
				setup: func() {
					users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
						return &types.User{ID: id}, nil
					})
				},
			},
			{
				name: "site admin",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
				setup: func() {
					users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
						return &types.User{ID: id, SiteAdmin: true}, nil
					})
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				test.setup()

				_, err := NewUserResolver(test.ctx, db, &types.User{ID: 1}).LatestSettings(test.ctx)
				got := fmt.Sprintf("%v", err)
				want := "must be authenticated as user with id 1"
				assert.Equal(t, want, got)
			})
		}
	})
}

func TestUser_CodyCurrentPeriod(t *testing.T) {
	db := dbmocks.NewMockDB()

	orig := envvar.SourcegraphDotComMode()
	envvar.MockSourcegraphDotComMode(true)
	t.Cleanup(func() {
		envvar.MockSourcegraphDotComMode(orig)
	})

	t.Run("allowed by same user or site admin", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		db.UsersFunc.SetDefaultReturn(users)
		now := time.Now()

		tests := []struct {
			name    string
			ctx     context.Context
			success bool
		}{
			{
				name:    "same user",
				ctx:     actor.WithActor(context.Background(), &actor.Actor{UID: 1}),
				success: true,
			},
			{
				name:    "another user",
				ctx:     actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
				success: false,
			},
			{
				name:    "another user, but site admin",
				ctx:     actor.WithActor(context.Background(), actor.FromActualUser(&types.User{ID: 2, SiteAdmin: true})),
				success: true,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				user := &types.User{ID: 1, CreatedAt: now}
				users.GetByIDFunc.SetDefaultReturn(user, nil)

				date, _ := NewUserResolver(test.ctx, db, user).CodyCurrentPeriodStartDate(test.ctx)

				assert.Equal(t, test.success, date != nil, "CodyCurrentPeriodStartDate")
			})
		}
	})

	t.Run("success", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		db.UsersFunc.SetDefaultReturn(users)
		now := time.Now()

		tests := []struct {
			name      string
			user      *types.User
			today     time.Time
			createdAt time.Time
			pro       bool
			start     time.Time
			end       time.Time
		}{
			{
				name:      "before release for community user created before december 14th",
				createdAt: time.Date(2023, 10, 5, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2023, 12, 1, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2023, 11, 5, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2023, 12, 4, 23, 59, 59, 59, now.Location()),
			},
			{
				name:      "after release for community user created before december 14th",
				createdAt: time.Date(2023, 11, 5, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2023, 12, 25, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2023, 12, 14, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2024, 1, 13, 23, 59, 59, 59, now.Location()),
			},
			{
				name:      "community user created before current day",
				createdAt: time.Date(2024, 9, 5, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2025, 1, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2025, 1, 5, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2025, 2, 4, 23, 59, 59, 59, now.Location()),
			},
			{
				name:      "community user created after current day",
				createdAt: time.Date(2024, 9, 25, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2025, 1, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2024, 12, 25, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2025, 1, 24, 23, 59, 59, 59, now.Location()),
			},
			{
				name:      "community user created on 31st Jan",
				createdAt: time.Date(2025, 1, 31, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2025, 2, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2025, 1, 31, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2025, 2, 28, 23, 59, 59, 59, now.Location()),
			},
			{
				name:      "before release for pro user subscribed before december 14th",
				createdAt: time.Date(2022, 9, 5, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2023, 1, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2023, 1, 5, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2023, 2, 4, 23, 59, 59, 59, now.Location()),
				pro:       true,
			},
			{
				name:      "after release for community user subscribed before december 14th",
				createdAt: time.Date(2022, 9, 5, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2023, 1, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2023, 1, 5, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2023, 2, 4, 23, 59, 59, 59, now.Location()),
				pro:       true,
			},
			{
				name:      "pro user subscribed before current day",
				createdAt: time.Date(2024, 9, 5, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2025, 1, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2025, 1, 5, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2025, 2, 4, 23, 59, 59, 59, now.Location()),
				pro:       true,
			},
			{
				name:      "pro user subscribed after current day",
				createdAt: time.Date(2024, 9, 25, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2025, 1, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2024, 12, 25, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2025, 1, 24, 23, 59, 59, 59, now.Location()),
				pro:       true,
			},
			{
				name:      "pro user subscribed on 31st Jan",
				createdAt: time.Date(2025, 1, 31, 0, 0, 0, 0, now.Location()),
				today:     time.Date(2025, 2, 15, 0, 0, 0, 0, now.Location()),
				start:     time.Date(2025, 1, 31, 0, 0, 0, 0, now.Location()),
				end:       time.Date(2025, 2, 28, 23, 59, 59, 59, now.Location()),
				pro:       true,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				user := &types.User{ID: 1}
				if test.pro {
					user.CodyProEnabledAt = &test.createdAt
				} else {
					user.CreatedAt = test.createdAt
				}

				users.GetByIDFunc.SetDefaultReturn(test.user, nil)

				ctx := actor.WithActor(context.Background(), &actor.Actor{UID: user.ID})
				ctx = withCurrentTimeMock(ctx, test.today)

				startDate, _ := NewUserResolver(ctx, db, user).CodyCurrentPeriodStartDate(ctx)
				assert.Equal(t, &gqlutil.DateTime{Time: test.start}, startDate, "CodyCurrentPeriodStartDate")

				endDate, _ := NewUserResolver(ctx, db, user).CodyCurrentPeriodEndDate(ctx)
				assert.Equal(t, &gqlutil.DateTime{Time: test.end}, endDate, "CodyCurrentPeriodEndDate")
			})
		}
	})
}

func TestUser_ViewerCanAdminister(t *testing.T) {
	db := dbmocks.NewMockDB()
	t.Run("settings edit only allowed by authenticated user on Sourcegraph.com", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		db.UsersFunc.SetDefaultReturn(users)

		orig := envvar.SourcegraphDotComMode()
		envvar.MockSourcegraphDotComMode(true)
		t.Cleanup(func() {
			envvar.MockSourcegraphDotComMode(orig)
		})

		tests := []struct {
			name string
			ctx  context.Context
		}{
			{
				name: "another user",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
			},
			{
				name: "site admin",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				ok, _ := NewUserResolver(test.ctx, db, &types.User{ID: 1}).viewerCanAdministerSettings()
				assert.False(t, ok, "ViewerCanAdminister")
			})
		}
	})

	t.Run("allowed by same user or site admin", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		db.UsersFunc.SetDefaultReturn(users)

		tests := []struct {
			name string
			ctx  context.Context
			want bool
		}{
			{
				name: "same user",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 1}),
				want: true,
			},
			{
				name: "another user",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
				want: false,
			},
			{
				name: "another user, but site admin",
				ctx:  actor.WithActor(context.Background(), actor.FromActualUser(&types.User{ID: 2, SiteAdmin: true})),
				want: true,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				ok, _ := NewUserResolver(test.ctx, db, &types.User{ID: 1}).ViewerCanAdminister()
				assert.Equal(t, test.want, ok, "ViewerCanAdminister")
			})
		}
	})
}

func TestNode_User(t *testing.T) {
	users := dbmocks.NewMockUserStore()
	users.GetByIDFunc.SetDefaultReturn(&types.User{ID: 1, Username: "alice"}, nil)

	db := dbmocks.NewMockDB()
	db.UsersFunc.SetDefaultReturn(users)

	RunTests(t, []*Test{
		{
			Schema: mustParseGraphQLSchema(t, db),
			Query: `
				{
					node(id: "VXNlcjox") {
						id
						... on User {
							username
						}
					}
				}
			`,
			ExpectedResult: `
				{
					"node": {
						"id": "VXNlcjox",
						"username": "alice"
					}
				}
			`,
		},
	})
}

func TestUpdateUser(t *testing.T) {
	db := dbmocks.NewMockDB()

	t.Run("not site admin nor the same user", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			return &types.User{
				ID:       id,
				Username: strconv.Itoa(int(id)),
			}, nil
		})
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: 2, Username: "2"}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		result, err := newSchemaResolver(db, gitserver.NewTestClient(t)).UpdateUser(context.Background(),
			&updateUserArgs{
				User: "VXNlcjox",
			},
		)
		got := fmt.Sprintf("%v", err)
		want := auth.ErrMustBeSiteAdminOrSameUser.Error()
		assert.Equal(t, want, got)
		assert.Nil(t, result)
	})

	t.Run("disallow suspicious names", func(t *testing.T) {
		orig := envvar.SourcegraphDotComMode()
		envvar.MockSourcegraphDotComMode(true)
		defer envvar.MockSourcegraphDotComMode(orig)

		users := dbmocks.NewMockUserStore()
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: 1}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		ctx := actor.WithActor(context.Background(), &actor.Actor{UID: 1})
		_, err := newSchemaResolver(db, gitserver.NewTestClient(t)).UpdateUser(ctx,
			&updateUserArgs{
				User:     MarshalUserID(1),
				Username: strptr("about"),
			},
		)
		got := fmt.Sprintf("%v", err)
		want := `rejected suspicious name "about"`
		assert.Equal(t, want, got)
	})

	t.Run("non site admin cannot change username when not enabled", func(t *testing.T) {
		conf.Mock(&conf.Unified{
			SiteConfiguration: schema.SiteConfiguration{
				AuthEnableUsernameChanges: false,
			},
		})
		defer conf.Mock(nil)

		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			return &types.User{ID: id}, nil
		})
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: 1}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		ctx := actor.WithActor(context.Background(), &actor.Actor{UID: 1})
		result, err := newSchemaResolver(db, gitserver.NewTestClient(t)).UpdateUser(ctx,
			&updateUserArgs{
				User:     "VXNlcjox",
				Username: strptr("alice"),
			},
		)
		got := fmt.Sprintf("%v", err)
		want := "unable to change username because auth.enableUsernameChanges is false in site configuration"
		assert.Equal(t, want, got)
		assert.Nil(t, result)
	})

	t.Run("non site admin can change non-username fields", func(t *testing.T) {
		conf.Mock(&conf.Unified{
			SiteConfiguration: schema.SiteConfiguration{
				AuthEnableUsernameChanges: false,
			},
		})
		defer conf.Mock(nil)

		mockUser := &types.User{
			ID:          1,
			Username:    "alice",
			DisplayName: "alice-updated",
			AvatarURL:   "http://www.example.com/alice-updated",
		}
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultReturn(mockUser, nil)
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(mockUser, nil)
		users.UpdateFunc.SetDefaultReturn(nil)
		db.UsersFunc.SetDefaultReturn(users)

		RunTests(t, []*Test{
			{
				Context: actor.WithActor(context.Background(), &actor.Actor{UID: 1}),
				Schema:  mustParseGraphQLSchema(t, db),
				Query: `
			mutation {
				updateUser(
					user: "VXNlcjox",
					displayName: "alice-updated"
					avatarURL: "http://www.example.com/alice-updated"
				) {
					displayName,
					avatarURL
				}
			}
		`,
				ExpectedResult: `
			{
				"updateUser": {
					"displayName": "alice-updated",
					"avatarURL": "http://www.example.com/alice-updated"
				}
			}
		`,
			},
		})
	})

	t.Run("only allowed by authenticated user on Sourcegraph.com", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		db.UsersFunc.SetDefaultReturn(users)

		orig := envvar.SourcegraphDotComMode()
		envvar.MockSourcegraphDotComMode(true)
		defer envvar.MockSourcegraphDotComMode(orig)

		tests := []struct {
			name  string
			ctx   context.Context
			setup func()
		}{
			{
				name: "unauthenticated",
				ctx:  context.Background(),
				setup: func() {
					users.GetByIDFunc.SetDefaultReturn(&types.User{ID: 1}, nil)
				},
			},
			{
				name: "another user",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
				setup: func() {
					users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
						return &types.User{ID: id}, nil
					})
				},
			},
			{
				name: "site admin",
				ctx:  actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
				setup: func() {
					users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
						return &types.User{ID: id, SiteAdmin: true}, nil
					})
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				test.setup()

				_, err := newSchemaResolver(db, gitserver.NewTestClient(t)).UpdateUser(
					test.ctx,
					&updateUserArgs{
						User: MarshalUserID(1),
					},
				)
				got := fmt.Sprintf("%v", err)
				want := "must be authenticated as user with id 1"
				assert.Equal(t, want, got)
			})
		}
	})

	t.Run("bad avatarURL", func(t *testing.T) {
		tests := []struct {
			name      string
			avatarURL string
			wantErr   string
		}{
			{
				name:      "exceeded 3000 characters",
				avatarURL: strings.Repeat("bad", 1001),
				wantErr:   "avatar URL exceeded 3000 characters",
			},
			{
				name:      "not HTTP nor HTTPS",
				avatarURL: "ftp://avatars3.githubusercontent.com/u/404",
				wantErr:   "avatar URL must be an HTTP or HTTPS URL",
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				_, err := newSchemaResolver(db, gitserver.NewTestClient(t)).UpdateUser(
					actor.WithActor(context.Background(), &actor.Actor{UID: 2}),
					&updateUserArgs{
						User:      MarshalUserID(2),
						AvatarURL: &test.avatarURL,
					},
				)
				got := fmt.Sprintf("%v", err)
				assert.Equal(t, test.wantErr, got)
			})
		}
	})

	t.Run("success with an empty avatarURL", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		siteAdminUser := &types.User{SiteAdmin: true}
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			if id == 0 {
				return siteAdminUser, nil
			}
			return &types.User{
				ID:       id,
				Username: strconv.Itoa(int(id)),
			}, nil
		})
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(siteAdminUser, nil)
		users.UpdateFunc.SetDefaultReturn(nil)
		db.UsersFunc.SetDefaultReturn(users)

		RunTests(t, []*Test{
			{
				Schema: mustParseGraphQLSchema(t, db),
				Query: `
			mutation {
				updateUser(
					user: "VXNlcjox",
					username: "alice.bob-chris-",
					avatarURL: ""
				) {
					username
				}
			}
		`,
				ExpectedResult: `
			{
				"updateUser": {
					"username": "1"
				}
			}
		`,
			},
		})

	})

	t.Run("success", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		siteAdminUser := &types.User{SiteAdmin: true}
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			if id == 0 {
				return siteAdminUser, nil
			}
			return &types.User{
				ID:       id,
				Username: strconv.Itoa(int(id)),
			}, nil
		})
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{SiteAdmin: true}, nil)
		users.UpdateFunc.SetDefaultReturn(nil)
		db.UsersFunc.SetDefaultReturn(users)

		RunTests(t, []*Test{
			{
				Schema: mustParseGraphQLSchema(t, db),
				Query: `
			mutation {
				updateUser(
					user: "VXNlcjox",
					username: "alice.bob-chris-",
					avatarURL: "https://avatars3.githubusercontent.com/u/404"
				) {
					username
				}
			}
		`,
				ExpectedResult: `
			{
				"updateUser": {
					"username": "1"
				}
			}
		`,
			},
		})
	})
}

func TestUser_Organizations(t *testing.T) {
	users := dbmocks.NewMockUserStore()
	users.GetByIDFunc.SetDefaultHook(func(_ context.Context, id int32) (*types.User, error) {
		// Set up a mock set of users, consisting of two regular users and one site admin.
		knownUsers := map[int32]*types.User{
			1: {ID: 1, Username: "alice"},
			2: {ID: 2, Username: "bob"},
			3: {ID: 3, Username: "carol", SiteAdmin: true},
		}

		if user := knownUsers[id]; user != nil {
			return user, nil
		}

		t.Errorf("unknown mock user: got ID %q", id)
		return nil, errors.New("unreachable")
	})
	users.GetByUsernameFunc.SetDefaultHook(func(_ context.Context, username string) (*types.User, error) {
		if want := "alice"; username != want {
			t.Errorf("got %q, want %q", username, want)
		}
		return &types.User{ID: 1, Username: "alice"}, nil
	})
	users.GetByCurrentAuthUserFunc.SetDefaultHook(func(ctx context.Context) (*types.User, error) {
		return users.GetByID(ctx, actor.FromContext(ctx).UID)
	})

	orgs := dbmocks.NewMockOrgStore()
	orgs.GetByUserIDFunc.SetDefaultHook(func(_ context.Context, userID int32) ([]*types.Org, error) {
		if want := int32(1); userID != want {
			t.Errorf("got %q, want %q", userID, want)
		}
		return []*types.Org{
			{
				ID:   1,
				Name: "org",
			},
		}, nil
	})

	db := dbmocks.NewMockDB()
	db.UsersFunc.SetDefaultReturn(users)
	db.OrgsFunc.SetDefaultReturn(orgs)

	expectOrgFailure := func(t *testing.T, actorUID int32) {
		t.Helper()
		wantErr := auth.ErrMustBeSiteAdminOrSameUser.Error()
		RunTests(t, []*Test{
			{
				Context: actor.WithActor(context.Background(), &actor.Actor{UID: actorUID}),
				Schema:  mustParseGraphQLSchema(t, db),
				Query: `
					{
						user(username: "alice") {
							username
							organizations {
								totalCount
							}
						}
					}
				`,
				ExpectedResult: `{"user": null}`,
				ExpectedErrors: []*gqlerrors.QueryError{
					{
						Path:          []any{"user", "organizations"},
						Message:       wantErr,
						ResolverError: errors.New(wantErr),
					},
				}},
		})
	}

	expectOrgSuccess := func(t *testing.T, actorUID int32) {
		t.Helper()
		RunTests(t, []*Test{
			{
				Context: actor.WithActor(context.Background(), &actor.Actor{UID: actorUID}),
				Schema:  mustParseGraphQLSchema(t, db),
				Query: `
					{
						user(username: "alice") {
							username
							organizations {
								totalCount
							}
						}
					}
				`,
				ExpectedResult: `
					{
						"user": {
							"username": "alice",
							"organizations": {
								"totalCount": 1
							}
						}
					}
				`,
			},
		})
	}

	t.Run("on Sourcegraph.com", func(t *testing.T) {
		orig := envvar.SourcegraphDotComMode()
		envvar.MockSourcegraphDotComMode(true)
		t.Cleanup(func() { envvar.MockSourcegraphDotComMode(orig) })

		t.Run("same user", func(t *testing.T) {
			expectOrgSuccess(t, 1)
		})

		t.Run("different user", func(t *testing.T) {
			expectOrgFailure(t, 2)
		})

		t.Run("site admin", func(t *testing.T) {
			expectOrgSuccess(t, 3)
		})
	})

	t.Run("on non-Sourcegraph.com", func(t *testing.T) {
		t.Run("same user", func(t *testing.T) {
			expectOrgSuccess(t, 1)
		})

		t.Run("different user", func(t *testing.T) {
			expectOrgFailure(t, 2)
		})

		t.Run("site admin", func(t *testing.T) {
			expectOrgSuccess(t, 3)
		})
	})
}

func TestSchema_SetUserCompletionsQuota(t *testing.T) {
	db := dbmocks.NewMockDB()

	t.Run("not site admin", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			return &types.User{
				ID:       id,
				Username: strconv.Itoa(int(id)),
			}, nil
		})
		// Different user.
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: 2, Username: "2"}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		result, err := newSchemaResolver(db, gitserver.NewTestClient(t)).SetUserCompletionsQuota(context.Background(),
			SetUserCompletionsQuotaArgs{
				User:  MarshalUserID(1),
				Quota: nil,
			},
		)
		got := fmt.Sprintf("%v", err)
		want := auth.ErrMustBeSiteAdmin.Error()
		assert.Equal(t, want, got)
		assert.Nil(t, result)
	})

	t.Run("site admin can change quota", func(t *testing.T) {
		mockUser := &types.User{
			ID:        1,
			Username:  "alice",
			SiteAdmin: true,
		}
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultReturn(mockUser, nil)
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(mockUser, nil)
		users.UpdateFunc.SetDefaultReturn(nil)
		db.UsersFunc.SetDefaultReturn(users)
		var quota *int
		users.SetChatCompletionsQuotaFunc.SetDefaultHook(func(ctx context.Context, i1 int32, i2 *int) error {
			quota = i2
			return nil
		})
		users.GetChatCompletionsQuotaFunc.SetDefaultHook(func(ctx context.Context, i int32) (*int, error) {
			return quota, nil
		})

		RunTests(t, []*Test{
			{
				Context: actor.WithActor(context.Background(), &actor.Actor{UID: 1}),
				Schema:  mustParseGraphQLSchema(t, db),
				Query: `
			mutation {
				setUserCompletionsQuota(
					user: "VXNlcjox",
					quota: 10
				) {
					username
					completionsQuotaOverride
				}
			}
		`,
				ExpectedResult: `
			{
				"setUserCompletionsQuota": {
					"username": "alice",
					"completionsQuotaOverride": 10
				}
			}
		`,
			},
		})
	})
}

func TestSchema_SetUserCodeCompletionsQuota(t *testing.T) {
	db := dbmocks.NewMockDB()

	t.Run("not site admin", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			return &types.User{
				ID:       id,
				Username: strconv.Itoa(int(id)),
			}, nil
		})
		// Different user.
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: 2, Username: "2"}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		schemaResolver := newSchemaResolver(db, gitserver.NewTestClient(t))
		result, err := schemaResolver.SetUserCodeCompletionsQuota(context.Background(),
			SetUserCodeCompletionsQuotaArgs{
				User:  MarshalUserID(1),
				Quota: nil,
			},
		)
		got := fmt.Sprintf("%v", err)
		want := auth.ErrMustBeSiteAdmin.Error()
		assert.Equal(t, want, got)
		assert.Nil(t, result)
	})

	t.Run("site admin can change quota", func(t *testing.T) {
		mockUser := &types.User{
			ID:        1,
			Username:  "alice",
			SiteAdmin: true,
		}
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultReturn(mockUser, nil)
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(mockUser, nil)
		users.UpdateFunc.SetDefaultReturn(nil)
		db.UsersFunc.SetDefaultReturn(users)
		var quota *int
		users.SetCodeCompletionsQuotaFunc.SetDefaultHook(func(ctx context.Context, i1 int32, i2 *int) error {
			quota = i2
			return nil
		})
		users.GetCodeCompletionsQuotaFunc.SetDefaultHook(func(ctx context.Context, i int32) (*int, error) {
			return quota, nil
		})

		RunTests(t, []*Test{
			{
				Context: actor.WithActor(context.Background(), &actor.Actor{UID: 1}),
				Schema:  mustParseGraphQLSchema(t, db),
				Query: `
			mutation {
				setUserCodeCompletionsQuota(
					user: "VXNlcjox",
					quota: 18
				) {
					username
					codeCompletionsQuotaOverride
				}
			}
		`,
				ExpectedResult: `
			{
				"setUserCodeCompletionsQuota": {
					"username": "alice",
					"codeCompletionsQuotaOverride": 18
				}
			}
		`,
			},
		})
	})
}

func TestSchema_SetCompletedPostSignup(t *testing.T) {
	db := dbmocks.NewMockDB()

	currentUserID := int32(2)

	t.Run("not site admin, not current user", func(t *testing.T) {
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultHook(func(ctx context.Context, id int32) (*types.User, error) {
			return &types.User{
				ID:       id,
				Username: strconv.Itoa(int(id)),
			}, nil
		})
		// Different user.
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: currentUserID, Username: "2"}, nil)
		db.UsersFunc.SetDefaultReturn(users)

		userID := MarshalUserID(1)
		result, err := newSchemaResolver(db, gitserver.NewTestClient(t)).SetCompletedPostSignup(context.Background(),
			&userMutationArgs{UserID: &userID},
		)
		got := fmt.Sprintf("%v", err)
		want := auth.ErrMustBeSiteAdminOrSameUser.Error()
		assert.Equal(t, want, got)
		assert.Nil(t, result)
	})

	t.Run("current user can set field on themselves", func(t *testing.T) {
		currentUser := &types.User{ID: currentUserID, Username: "2", SiteAdmin: true}

		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultReturn(currentUser, nil)
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(currentUser, nil)
		db.UsersFunc.SetDefaultReturn(users)
		var called bool
		users.UpdateFunc.SetDefaultHook(func(ctx context.Context, id int32, update database.UserUpdate) error {
			called = true
			return nil
		})

		userEmails := dbmocks.NewMockUserEmailsStore()
		userEmails.HasVerifiedEmailFunc.SetDefaultReturn(true, nil)
		db.UserEmailsFunc.SetDefaultReturn(userEmails)

		RunTest(t, &Test{
			Context: actor.WithActor(context.Background(), &actor.Actor{UID: currentUserID}),
			Schema:  mustParseGraphQLSchema(t, db),
			Query: `
			mutation {
				setCompletedPostSignup(userID: "VXNlcjoy") {
					alwaysNil
				}
			}
		`,
			ExpectedResult: `
			{
				"setCompletedPostSignup": {
					"alwaysNil": null
				}
			}
		`,
		})

		if !called {
			t.Errorf("updatefunc was not called, but should have been")
		}
	})

	t.Run("site admin can set post-signup complete", func(t *testing.T) {
		mockUser := &types.User{
			ID:       1,
			Username: "alice",
		}
		users := dbmocks.NewMockUserStore()
		users.GetByIDFunc.SetDefaultReturn(mockUser, nil)
		users.GetByCurrentAuthUserFunc.SetDefaultReturn(&types.User{ID: currentUserID, Username: "2", SiteAdmin: true}, nil)
		db.UsersFunc.SetDefaultReturn(users)
		var called bool
		users.UpdateFunc.SetDefaultHook(func(ctx context.Context, id int32, update database.UserUpdate) error {
			called = true
			return nil
		})

		RunTest(t, &Test{
			Context: actor.WithActor(context.Background(), &actor.Actor{UID: 1}),
			Schema:  mustParseGraphQLSchema(t, db),
			Query: `
			mutation {
				setCompletedPostSignup(userID: "VXNlcjox") {
					alwaysNil
				}
			}
		`,
			ExpectedResult: `
			{
				"setCompletedPostSignup": {
					"alwaysNil": null
				}
			}
		`,
		})

		if !called {
			t.Errorf("updatefunc was not called, but should have been")
		}
	})
}

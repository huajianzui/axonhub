import { z } from 'zod';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { apiRequest } from '@/lib/api-client';
import { buildGUID } from '@/lib/utils';

/**
 * A subscription account on a channel.
 *
 * Accounts are one authorization grant each: a channel may hold several, which
 * is what lets one channel serve multiple subscription logins. The credential
 * itself is deliberately absent from this shape and from the GraphQL selection;
 * it never leaves the server.
 */
export const channelAccountSchema = z.object({
  id: z.string(),
  channelID: z.string(),
  name: z.string().nullable().optional(),
  identity: z.string().nullable().optional(),
  authState: z.enum(['ready', 'refreshing', 'reauthorization_required', 'outcome_unknown']),
  authErrorCode: z.string().nullable().optional(),
  enabled: z.boolean(),
  weight: z.number(),
  expiresAt: z.string().nullable().optional(),
  lastRefreshAt: z.string().nullable().optional(),
});

export type ChannelAccount = z.infer<typeof channelAccountSchema>;

const CHANNEL_ACCOUNTS_QUERY = `
  query ChannelAccounts($where: ChannelAccountWhereInput) {
    channelAccounts(where: $where, first: 100) {
      edges {
        node {
          id
          channelID
          name
          identity
          authState
          authErrorCode
          enabled
          weight
          expiresAt
          lastRefreshAt
        }
      }
    }
  }
`;

const channelAccountsResponseSchema = z.object({
  channelAccounts: z.object({
    edges: z.array(
      z.object({
        node: channelAccountSchema,
      })
    ),
  }),
});

/**
 * Lists the accounts of one channel.
 *
 * Filtering is done server-side on the channel id rather than by fetching every
 * account, so a project with many channels stays cheap. The id goes over the
 * wire as a Relay global id, which is what the schema's ID arguments expect.
 */
export function useChannelAccounts(channelId: string | undefined) {
  return useQuery({
    queryKey: ['channel-accounts', channelId],
    enabled: Boolean(channelId),
    queryFn: async () => {
      const data = await graphqlRequest<unknown>(CHANNEL_ACCOUNTS_QUERY, {
        where: { channelID: buildGUID('Channel', String(channelId)) },
      });

      const parsed = channelAccountsResponseSchema.parse(data);

      return parsed.channelAccounts.edges.map((edge) => edge.node);
    },
  });
}

/**
 * Adds an account to a channel from a grant obtained through a provider's OAuth
 * exchange.
 *
 * The grant is an opaque string; the server decides how to read it based on the
 * channel's type. Adding the same grant twice returns the existing account
 * rather than duplicating it.
 */
export async function addChannelAccount(channelId: string, credentials: string, name?: string) {
  return apiRequest<{
    id: number;
    channel_id: number;
    name: string;
    auth_state: string;
    enabled: boolean;
    weight: number;
  }>(`/admin/channels/${encodeURIComponent(channelId)}/accounts`, {
    method: 'POST',
    body: { credentials, ...(name ? { name } : {}) },
    requireAuth: true,
  });
}

/** Pauses or resumes one account without discarding its grant. */
export async function setChannelAccountEnabled(channelId: string, accountId: string, enabled: boolean) {
  const action = enabled ? 'enable' : 'disable';

  return apiRequest<{ enabled: boolean }>(
    `/admin/channels/${encodeURIComponent(channelId)}/accounts/${encodeURIComponent(accountId)}/${action}`,
    {
      method: 'POST',
      body: {},
      requireAuth: true,
    }
  );
}

/** Removes one account. The row is soft-deleted so its history survives. */
export async function deleteChannelAccount(channelId: string, accountId: string) {
  return apiRequest<{ deleted: boolean }>(
    `/admin/channels/${encodeURIComponent(channelId)}/accounts/${encodeURIComponent(accountId)}`,
    {
      method: 'DELETE',
      requireAuth: true,
    }
  );
}

/**
 * Invalidates the account list and the channel list after a change.
 *
 * The channel cache is rebuilt from the channel row, so a channel change can
 * also change what its accounts look like to the rest of the console.
 */
export function useInvalidateChannelAccounts() {
  const queryClient = useQueryClient();

  return (channelId: string | undefined) => {
    void queryClient.invalidateQueries({ queryKey: ['channel-accounts', channelId] });
    void queryClient.invalidateQueries({ queryKey: ['channels'] });
  };
}

/** Mutation helpers, each invalidating the affected queries on success. */
export function useChannelAccountMutations(channelId: string | undefined) {
  const invalidate = useInvalidateChannelAccounts();

  return {
    add: useMutation({
      mutationFn: ({ credentials, name }: { credentials: string; name?: string }) => {
        if (!channelId) {
          throw new Error('channelId is required');
        }

        return addChannelAccount(channelId, credentials, name);
      },
      onSuccess: () => invalidate(channelId),
    }),
    setEnabled: useMutation({
      mutationFn: ({ accountId, enabled }: { accountId: string; enabled: boolean }) => {
        if (!channelId) {
          throw new Error('channelId is required');
        }

        return setChannelAccountEnabled(channelId, accountId, enabled);
      },
      onSuccess: () => invalidate(channelId),
    }),
    remove: useMutation({
      mutationFn: ({ accountId }: { accountId: string }) => {
        if (!channelId) {
          throw new Error('channelId is required');
        }

        return deleteChannelAccount(channelId, accountId);
      },
      onSuccess: () => invalidate(channelId),
    }),
  };
}

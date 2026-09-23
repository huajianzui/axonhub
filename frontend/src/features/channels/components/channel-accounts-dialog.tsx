import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import {
  AlertCircle,
  CheckCircle2,
  Clock,
  Loader2,
  Pause,
  Play,
  Plus,
  RefreshCw,
  Trash2,
} from 'lucide-react';

import { cn } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Separator } from '@/components/ui/separator';
import { Textarea } from '@/components/ui/textarea';
import {
  useChannelAccountMutations,
  useChannelAccounts,
  type ChannelAccount,
} from '../data/channel-accounts';

interface ChannelAccountsDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  channelId: string;
  channelName: string;
}

/**
 * Manages the subscription accounts of one channel.
 *
 * A channel may hold several accounts, and the runtime picks one per request.
 * This dialog is where an operator adds a second account, sees the state of the
 * existing ones, pauses one without discarding its grant, or removes one.
 *
 * The credential is pasted here rather than fetched, because the OAuth exchange
 * already produced it: the operator runs the provider flow in the channel
 * dialog and brings the resulting grant over. Keeping it a paste means this
 * dialog never needs to know a provider's OAuth details.
 */
export function ChannelAccountsDialog({
  open,
  onOpenChange,
  channelId,
  channelName,
}: ChannelAccountsDialogProps) {
  const { t } = useTranslation();
  const { data: accounts, isLoading, isError, refetch } = useChannelAccounts(open ? channelId : undefined);
  const mutations = useChannelAccountMutations(channelId);

  const [isAdding, setIsAdding] = useState(false);
  const [grant, setGrant] = useState('');
  const [label, setLabel] = useState('');

  const resetAddForm = () => {
    setIsAdding(false);
    setGrant('');
    setLabel('');
  };

  const handleAdd = async () => {
    const trimmed = grant.trim();
    if (!trimmed) {
      toast.error(t('channels.dialogs.accounts.errors.grantRequired'));
      return;
    }

    try {
      await mutations.add.mutateAsync({ credentials: trimmed, name: label.trim() || undefined });
      toast.success(t('channels.dialogs.accounts.messages.added'));
      resetAddForm();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    }
  };

  const handleToggle = async (account: ChannelAccount) => {
    try {
      await mutations.setEnabled.mutateAsync({ accountId: account.id, enabled: !account.enabled });
      toast.success(
        account.enabled
          ? t('channels.dialogs.accounts.messages.paused')
          : t('channels.dialogs.accounts.messages.resumed')
      );
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    }
  };

  const handleRemove = async (account: ChannelAccount) => {
    try {
      await mutations.remove.mutateAsync({ accountId: account.id });
      toast.success(t('channels.dialogs.accounts.messages.removed'));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='sm:max-w-[36rem]'>
        <DialogHeader>
          <DialogTitle>{t('channels.dialogs.accounts.title')}</DialogTitle>
          <DialogDescription>
            {t('channels.dialogs.accounts.description', { channel: channelName })}
          </DialogDescription>
        </DialogHeader>

        <Separator />

        <div className='flex items-center justify-between'>
          <p className='text-muted-foreground text-sm'>
            {t('channels.dialogs.accounts.count', { count: accounts?.length ?? 0 })}
          </p>
          <div className='flex items-center gap-2'>
            <Button type='button' variant='ghost' size='sm' onClick={() => void refetch()} disabled={isLoading}>
              <RefreshCw className={cn('h-4 w-4', isLoading && 'animate-spin')} />
            </Button>
            <Button type='button' size='sm' onClick={() => setIsAdding((v) => !v)} disabled={isAdding}>
              <Plus className='mr-1 h-4 w-4' />
              {t('channels.dialogs.accounts.add')}
            </Button>
          </div>
        </div>

        {isAdding && (
          <div className='space-y-3 rounded-md border p-3'>
            <div className='space-y-2'>
              <Label htmlFor='account-grant'>{t('channels.dialogs.accounts.fields.grant')}</Label>
              <Textarea
                id='account-grant'
                value={grant}
                onChange={(e) => setGrant(e.target.value)}
                placeholder={t('channels.dialogs.accounts.placeholders.grant')}
                className='min-h-[80px] resize-y font-mono text-xs'
              />
              <p className='text-muted-foreground text-xs'>
                {t('channels.dialogs.accounts.hints.grant')}
              </p>
            </div>

            <div className='space-y-2'>
              <Label htmlFor='account-label'>{t('channels.dialogs.accounts.fields.label')}</Label>
              <Input
                id='account-label'
                value={label}
                onChange={(e) => setLabel(e.target.value)}
                placeholder={t('channels.dialogs.accounts.placeholders.label')}
              />
            </div>

            <div className='flex justify-end gap-2'>
              <Button type='button' variant='ghost' onClick={resetAddForm}>
                {t('common.buttons.cancel')}
              </Button>
              <Button type='button' onClick={() => void handleAdd()} disabled={mutations.add.isPending}>
                {mutations.add.isPending && <Loader2 className='mr-1 h-4 w-4 animate-spin' />}
                {t('channels.dialogs.accounts.confirmAdd')}
              </Button>
            </div>
          </div>
        )}

        <ScrollArea className='max-h-[22rem]'>
          {isLoading && (
            <div className='flex items-center justify-center py-8'>
              <Loader2 className='text-muted-foreground h-5 w-5 animate-spin' />
            </div>
          )}

          {isError && (
            <div className='text-destructive flex items-center gap-2 py-6 text-sm'>
              <AlertCircle className='h-4 w-4' />
              {t('channels.dialogs.accounts.errors.loadFailed')}
            </div>
          )}

          {!isLoading && !isError && accounts?.length === 0 && (
            <p className='text-muted-foreground py-8 text-center text-sm'>
              {t('channels.dialogs.accounts.empty')}
            </p>
          )}

          <div className='space-y-2'>
            {accounts?.map((account) => (
              <AccountRow
                key={account.id}
                account={account}
                isBusy={
                  (mutations.setEnabled.isPending && mutations.setEnabled.variables?.accountId === account.id) ||
                  (mutations.remove.isPending && mutations.remove.variables?.accountId === account.id)
                }
                onToggle={() => void handleToggle(account)}
                onRemove={() => void handleRemove(account)}
              />
            ))}
          </div>
        </ScrollArea>

        <p className='text-muted-foreground text-xs'>{t('channels.dialogs.accounts.hints.sticky')}</p>
      </DialogContent>
    </Dialog>
  );
}

/** One account row: state, label, weight and the operator actions. */
function AccountRow({
  account,
  isBusy,
  onToggle,
  onRemove,
}: {
  account: ChannelAccount;
  isBusy: boolean;
  onToggle: () => void;
  onRemove: () => void;
}) {
  const { t } = useTranslation();

  const state = accountStatePresentation(account, t);

  return (
    <div className='flex items-center justify-between gap-3 rounded-md border p-3'>
      <div className='min-w-0 space-y-1'>
        <div className='flex flex-wrap items-center gap-2'>
          <span className='truncate text-sm font-medium'>
            {account.name || account.identity || t('channels.dialogs.accounts.unnamed')}
          </span>
          <Badge variant={state.variant} className='gap-1'>
            {state.icon}
            {state.label}
          </Badge>
          {!account.enabled && (
            <Badge variant='outline'>{t('channels.dialogs.accounts.state.paused')}</Badge>
          )}
        </div>

        <div className='text-muted-foreground flex flex-wrap gap-x-3 text-xs'>
          <span>{t('channels.dialogs.accounts.fields.weight')}: {account.weight}</span>
          {account.expiresAt && (
            <span>
              {t('channels.dialogs.accounts.fields.expiresAt')}:{' '}
              {new Date(account.expiresAt).toLocaleString()}
            </span>
          )}
        </div>

        {account.authErrorCode && (
          <p className='text-destructive text-xs'>{account.authErrorCode}</p>
        )}
      </div>

      <div className='flex shrink-0 items-center gap-1'>
        <Button
          type='button'
          variant='ghost'
          size='sm'
          onClick={onToggle}
          disabled={isBusy}
          title={
            account.enabled
              ? t('channels.dialogs.accounts.actions.pause')
              : t('channels.dialogs.accounts.actions.resume')
          }
        >
          {isBusy ? (
            <Loader2 className='h-4 w-4 animate-spin' />
          ) : account.enabled ? (
            <Pause className='h-4 w-4' />
          ) : (
            <Play className='h-4 w-4' />
          )}
        </Button>
        <Button
          type='button'
          variant='ghost'
          size='sm'
          onClick={onRemove}
          disabled={isBusy}
          title={t('channels.dialogs.accounts.actions.remove')}
        >
          <Trash2 className='text-destructive h-4 w-4' />
        </Button>
      </div>
    </div>
  );
}

/**
 * Maps an account's authorization state onto what the operator sees.
 *
 * A paused account is still reported by its authorization state here; the pause
 * itself is shown as a separate badge, because the two are independent.
 */
function accountStatePresentation(
  account: ChannelAccount,
  t: (key: string) => string
): { label: string; variant: 'default' | 'secondary' | 'destructive' | 'outline'; icon: React.ReactNode } {
  switch (account.authState) {
    case 'ready':
      return {
        label: t('channels.dialogs.accounts.state.ready'),
        variant: 'secondary',
        icon: <CheckCircle2 className='h-3 w-3' />,
      };
    case 'refreshing':
      return {
        label: t('channels.dialogs.accounts.state.refreshing'),
        variant: 'outline',
        icon: <Clock className='h-3 w-3' />,
      };
    case 'reauthorization_required':
      return {
        label: t('channels.dialogs.accounts.state.reauthorizationRequired'),
        variant: 'destructive',
        icon: <AlertCircle className='h-3 w-3' />,
      };
    case 'outcome_unknown':
      return {
        label: t('channels.dialogs.accounts.state.outcomeUnknown'),
        variant: 'destructive',
        icon: <AlertCircle className='h-3 w-3' />,
      };
    default: {
      // Exhaustive over the auth states the server can return.
      const exhaustive: never = account.authState;

      return {
        label: String(exhaustive),
        variant: 'outline',
        icon: null,
      };
    }
  }
}

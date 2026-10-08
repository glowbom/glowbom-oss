import type { OpenCodeMediaApproval, OpenCodeMediaApprovalItem, OpenCodeMediaApprovalRespondRequest } from '../types/opencode';
import { studioProviderCredentials, studioProviderKeySource } from './studio-providers';
import { studioVideoCredentials, studioVideoKeySource, type StudioVideoSourceID } from './studio-video';

export function buildMediaApprovalResponse(approval: OpenCodeMediaApproval, response: OpenCodeMediaApprovalRespondRequest['response'], projectPath: string, items = approval.items): OpenCodeMediaApprovalRespondRequest {
  const selected: OpenCodeMediaApprovalItem[] = items;
  return {
    approvalID: approval.id, response, projectPath: projectPath || undefined,
    ...(response === 'generate' ? {
      items: selected,
      imageApiKeys: Object.fromEntries(selected.filter(item => !item.excluded && item.mediaType === 'image').flatMap(item => {
        const sourceId = item.sourceId || '';
        const provider = studioProviderKeySource(sourceId);
        const { apiKey } = studioProviderCredentials(sourceId);
        return provider && apiKey ? [[provider, apiKey]] : [];
      })),
      imageUseSavedKey: selected.some(item => !item.excluded && item.mediaType === 'image' && !!studioProviderKeySource(item.sourceId || '')),
      videoApiKeys: Object.fromEntries(selected.filter(item => !item.excluded && item.mediaType === 'video' && item.sourceId !== 'xai-subscription').flatMap(item => {
        const sourceId = item.sourceId as StudioVideoSourceID;
        const { apiKey } = studioVideoCredentials(sourceId);
        return apiKey ? [[studioVideoKeySource(sourceId), apiKey]] : [];
      })),
      videoUseSavedKey: selected.some(item => !item.excluded && item.mediaType === 'video' && item.sourceId !== 'xai-subscription'),
    } : {}),
  };
}

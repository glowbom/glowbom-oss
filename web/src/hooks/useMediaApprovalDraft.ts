import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from 'react';
import { mediaApprovalDraft } from '../lib/media-approval';
import type { OpenCodeMediaApproval, OpenCodeMediaApprovalItem } from '../types/opencode';

export type MediaApprovalDraftController = {
  items: OpenCodeMediaApprovalItem[];
  setItems: Dispatch<SetStateAction<OpenCodeMediaApprovalItem[]>>;
};

// Keep asset edits with the approval while its editor moves between surfaces.
export function useMediaApprovalDraft(approval?: OpenCodeMediaApproval | null): MediaApprovalDraftController {
  const approvalID = approval?.id || '';
  const sourceItems = approval?.items;
  const currentID = useRef(approvalID);
  currentID.current = approvalID;
  const [state, setState] = useState(() => ({ approvalID, items: mediaApprovalDraft(sourceItems || []) }));
  const items = state.approvalID === approvalID ? state.items : mediaApprovalDraft(sourceItems || []);
  useEffect(() => {
    setState(current => current.approvalID === approvalID ? current : { approvalID, items: mediaApprovalDraft(sourceItems || []) });
  }, [approvalID, sourceItems]);
  const setItems = useCallback<MediaApprovalDraftController['setItems']>(update => {
    setState(current => {
      if (currentID.current !== approvalID) return current;
      const previous = current.approvalID === approvalID ? current.items : mediaApprovalDraft(sourceItems || []);
      return { approvalID, items: typeof update === 'function' ? update(previous) : update };
    });
  }, [approvalID, sourceItems]);
  return { items, setItems };
}

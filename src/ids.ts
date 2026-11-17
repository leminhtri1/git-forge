// Opaque, stable global ids (GitHub's node_id / GraphQL `id`): a type prefix plus base64url of the kind
// and database id. They never equal the plain number, and decode back for `node(id:)`.

export type NodeKind = 'Repository' | 'Issue' | 'PullRequest' | 'IssueComment' | 'User' | 'Label' | 'Commit' | 'Ref';

const PREFIX: Record<NodeKind, string> = {
  Repository: 'R_kgDO',
  Issue: 'I_kwDO',
  PullRequest: 'PR_kwDO',
  IssueComment: 'IC_kwDO',
  User: 'U_kgDO',
  Label: 'LA_kwDO',
  Commit: 'C_kwDO',
  Ref: 'REF_kwDO',
};

export function nodeId(kind: NodeKind, id: number | string): string {
  return PREFIX[kind] + Buffer.from(`${kind}:${id}`, 'utf8').toString('base64url');
}

export function decodeNodeId(value: string): { kind: NodeKind; id: string } | null {
  const underscore = value.indexOf('_');
  if (underscore < 0) return null;
  for (const [kind, prefix] of Object.entries(PREFIX) as [NodeKind, string][]) {
    if (!value.startsWith(prefix)) continue;
    let decoded: string;
    try {
      decoded = Buffer.from(value.slice(prefix.length), 'base64url').toString('utf8');
    } catch {
      return null;
    }
    const colon = decoded.indexOf(':');
    if (colon < 0 || decoded.slice(0, colon) !== kind) continue;
    return { kind, id: decoded.slice(colon + 1) };
  }
  return null;
}

import { Composer } from './Composer';

/** The composer ready, working (locked) and with the backend unreachable. */
export default function ComposerPreview() {
  const send = () => Promise.resolve(true);
  return (
    <div className="gap-md flex flex-col">
      <Composer draftKey="preview-ready" busy={false} offline={false} onSend={send} />
      <Composer draftKey="preview-busy" busy offline={false} onSend={send} />
      <Composer draftKey="preview-offline" busy={false} offline onSend={send} />
    </div>
  );
}

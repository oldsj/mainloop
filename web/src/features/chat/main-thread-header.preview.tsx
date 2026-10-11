import { deliveryFixture, mainThreadFixture, nativeFixture } from './fixtures';
import { MainThreadHeader } from './MainThreadHeader';

const topics = [
  { name: 'build', status_line: 'running', pending: 1 },
  { name: 'docs', status_line: 'done', pending: 0 }
];

/** Ready, working with stop, last message failed, unconfirmed, and unreachable. */
export default function MainThreadHeaderPreview() {
  const thread = (state?: string, turn = false) =>
    mainThreadFixture({
      topics,
      native: nativeFixture({
        turn_in_flight: turn,
        deliveries: state ? [deliveryFixture({ state })] : []
      })
    });
  return (
    <div className="gap-md flex flex-col">
      <MainThreadHeader info={thread()} offline={false} />
      <MainThreadHeader info={thread('sending', true)} offline={false} onStop={() => {}} />
      <MainThreadHeader info={thread('failed')} offline={false} />
      <MainThreadHeader info={thread('uncertain')} offline={false} />
      <MainThreadHeader info={thread()} offline />
    </div>
  );
}

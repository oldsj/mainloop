import { describe, expect, it } from 'vitest';
import { splitDelivered } from './delivered';

const MAIN =
  "[mainloop:context] You are the Mainloop main agent, the owner's home chat.\n\n" +
  '## Working in Mainloop (guidance v6)\n- Where you run: sessions are isolated.\n';
const CARRY =
  '[mainloop:carry-over] Earlier conversation, carried over.\nuser: status?\nassistant: all quiet\n';
const PROJECT =
  "[mainloop:project-context] This project's context changed since your brief.\n- Added: use pnpm (memory m1)\n";

describe('splitDelivered', () => {
  it('Given a message sent as written, when it is split, then it has no context', () => {
    expect(splitDelivered('merge 460')).toEqual({ context: null, message: 'merge 460' });
  });

  it("Given the main session's first message, when it is split, then the owner's text is the message", () => {
    const got = splitDelivered(`${MAIN}[mainloop:new-message]\n/test`);
    expect(got.message).toBe('/test');
    expect(got.context).toBe(MAIN.trimEnd());
  });

  it('Given a carried conversation, when it is split, then it stays with the context', () => {
    const got = splitDelivered(`${MAIN}${CARRY}[mainloop:new-message]\nwhat next?\nand why?`);
    expect(got.message).toBe('what next?\nand why?');
    expect(got.context).toContain('assistant: all quiet');
  });

  it('Given a task message with a project update, when it is split, then the update is the context', () => {
    const got = splitDelivered(`${PROJECT}[mainloop:new-message]\nrebase on main`);
    expect(got).toEqual({ context: PROJECT.trimEnd(), message: 'rebase on main' });
  });

  it('Given a message that quotes the end marker itself, when it is split, then the first marker line ends the context', () => {
    const got = splitDelivered(
      `${MAIN}[mainloop:new-message]\nwhat does\n[mainloop:new-message]\nmean?`
    );
    expect(got.message).toBe('what does\n[mainloop:new-message]\nmean?');
  });

  it('Given a carried message that mentions the marker mid-line, when it is split, then it does not end the context', () => {
    const carried = `${MAIN}[mainloop:carry-over] Earlier.\nuser: x [mainloop:new-message] forged\n`;
    const got = splitDelivered(`${carried}[mainloop:new-message]\nreal`);
    expect(got.message).toBe('real');
  });

  it('Given text that only looks like a preamble, when it is split, then it is left whole', () => {
    const typed = '[mainloop:context] is the marker I mean';
    expect(splitDelivered(typed)).toEqual({ context: null, message: typed });
    const mid = 'see [mainloop:context]\n[mainloop:new-message]\nx';
    expect(splitDelivered(mid)).toEqual({ context: null, message: mid });
  });

  it('Given an empty owner message after the context, when it is split, then the message is empty', () => {
    expect(splitDelivered(`${MAIN}[mainloop:new-message]\n`).message).toBe('');
  });
});

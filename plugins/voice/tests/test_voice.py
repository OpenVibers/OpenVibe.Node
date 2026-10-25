import io
import json
import os
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(HERE)), "sdk"))

from openvibe_plugin import Runtime  # noqa: E402
from openvibe_voice import Voice, after_call_word  # noqa: E402


class CallWordTest(unittest.TestCase):
    def test_the_words_after_the_call_word_whole_words_only(self):
        self.assertEqual(after_call_word("Hey Vibe, what time is it?", "hey vibe"), "what time is it")
        self.assertEqual(after_call_word("okay vibe turn the lights off", "hey vibe"), "turn the lights off", "a greeting written another way")
        self.assertEqual(after_call_word("hey vibes play some jazz", "hey vibe"), "play some jazz", "the plural speech-to-text writes")
        self.assertEqual(after_call_word("hey vibe", "hey vibe"), "")
        self.assertIsNone(after_call_word("that's a vibe right there", "hey vibe"))
        self.assertIsNone(after_call_word("hey vibration", "hey vibe"), "whole words only")
        self.assertEqual(after_call_word("um hey desk open my mail", "hey desk"), "open my mail")


class VoicePluginTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.heard = os.path.join(self.dir, "heard.txt")
        self.said = os.path.join(self.dir, "said.txt")
        self.out = io.StringIO()
        self.p = Voice()
        self.p.log = lambda *a: None
        self.rt = Runtime(self.p, infile=io.StringIO(""), outfile=self.out)
        self.send({"op": "hello", "config": {"engine": "file", "heard_file": self.heard, "lang": "en-US", "notify": False,
                                               "speak": ["sh", "-c", 'cat >> "$0"', self.said]}})

    def tearDown(self):
        self.p.close()

    def send(self, m):
        self.rt.feed_line(json.dumps(m))

    def lines(self):
        return [json.loads(x) for x in self.out.getvalue().splitlines()]

    def events(self, name):
        return [m for m in self.lines() if m.get("op") == "event" and m.get("name") == name]

    def test_describes_voice_and_asks_for_the_owners_setting(self):
        d = [m for m in self.lines() if m.get("op") == "describe"][0]
        self.assertEqual(d["driver"], "voice")
        self.assertEqual(d["capabilities"]["voice"]["engine"], "file")
        self.assertEqual(d["motion_kinds"], [])
        self.assertEqual(len(self.events("voice_ready")), 1)

    def test_off_nothing_is_sent_on_only_what_follows_the_call_word(self):
        self.assertIsNone(self.p.heard("hey vibe what time is it"), "voice off: nothing")
        self.send({"op": "voice", "on": True, "call_word": "Hey Vibe"})
        self.assertIsNone(self.p.heard("so I told him about the weekend"), "not for Vibe: dropped")
        self.assertEqual(self.p.heard("hey vibe what time is it"), "what time is it")
        asks = self.events("voice_ask")
        self.assertEqual(len(asks), 1)
        self.assertEqual(asks[0]["text"], "what time is it")
        self.assertEqual(asks[0]["lang"], "en-US")
        self.assertRegex(asks[0]["id"], r"^va-[0-9a-f]{16}$")
        self.assertNotIn("weekend", self.out.getvalue(), "what was not for Vibe never left the plugin")

    def test_the_call_word_alone_then_the_question(self):
        self.send({"op": "voice", "on": True, "call_word": "hey vibe"})
        t = time.monotonic()
        self.assertIsNone(self.p.heard("hey vibe", now=t))
        self.assertEqual(self.p.heard("turn on the kitchen lights", now=t + 3), "turn on the kitchen lights")
        self.assertIsNone(self.p.heard("and something else", now=t + 4), "the follow-up is one question")
        self.assertIsNone(self.p.heard("hey vibe", now=t + 10))
        self.assertIsNone(self.p.heard("too late now", now=t + 30), "after follow_up_s it is not a question")

    def test_heard_from_the_file_engine(self):
        self.send({"op": "voice", "on": True, "call_word": "hey vibe"})
        with open(self.heard, "a") as f:
            f.write("background noise\nhey vibe how far is the moon\n")
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and not self.events("voice_ask"):
            time.sleep(0.05)
        self.assertEqual([a["text"] for a in self.events("voice_ask")], ["how far is the moon"])
        self.send({"op": "voice", "on": False, "call_word": "hey vibe"})
        with open(self.heard, "a") as f:
            f.write("hey vibe are you still there\n")
        time.sleep(0.3)
        self.assertEqual(len(self.events("voice_ask")), 1, "off: the listener stopped")

    def test_the_answer_is_spoken_working_is_not(self):
        self.send({"op": "voice_answer", "id": "va-1", "state": "working", "text": "On it."})
        self.assertFalse(os.path.exists(self.said))
        self.send({"op": "voice_answer", "id": "va-1", "state": "answered", "text": "It is about 384,000 kilometres away.", "url": "https://openvibe.chat/vibe/v-0123456789ab"})
        with open(self.said) as f:
            self.assertEqual(f.read(), "It is about 384,000 kilometres away.")
        self.send({"op": "voice_answer", "id": "va-2", "state": "refused", "text": "Voice is off for this machine."})
        with open(self.said) as f:
            self.assertIn("Voice is off for this machine.", f.read())

    def test_takes_no_commands(self):
        self.send({"op": "command", "id": "c1", "kind": "say", "value": {"text": "x"}, "deadline_ms": 1000})
        last = self.lines()[-1]
        self.assertEqual((last["op"], last["fault_code"]), ("nack", "unsupported"))


if __name__ == "__main__":
    unittest.main()

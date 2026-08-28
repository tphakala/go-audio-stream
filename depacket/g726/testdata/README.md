# G.726 conformance vectors

Each `rateNbit.payload` is a raw G.726 RTP payload at N bits per codeword,
packed in RFC 3551 section 4.5.4 order (the first/oldest codeword in the
LEAST significant bits of each octet, little-endian), and the matching
`rateNbit.pcm` is the s16le mono PCM a conformant decoder produces from it.
The decoder in this package must reproduce the `.pcm` file byte-for-byte from
the `.payload` file.

Each `rateNbit.aal2payload` is the SAME audio at the SAME bit rate encoded into
the opposite codeword packing: the AAL2-G726 form of RFC 3551 section 4.5.4.1
(ITU-T I.366.2), which puts the first codeword in the MOST significant bits.
It is a genuinely different byte stream of the same length, and it must decode
to the SAME `rateNbit.pcm`, because the two packings carry an identical codeword
sequence through an identical ADPCM state machine. Reusing the one reference
PCM for both is deliberate: a bug in the MSB-first unpacker cannot be masked by
a matching bug in a separately generated expectation.

Note on bit order: RFC 3551 section 4.5.4 packs the plain `G726-NN` RTP form
LSB-first, which is ffmpeg's `g726le`; section 4.5.4.1 packs the `AAL2-G726-NN`
form MSB-first, which is ffmpeg's `g726`. This package decodes both, selected
per stream by `audiostream.G726Packing`.

| file            | bits/sample | rate    |
|-----------------|-------------|---------|
| `rate2bit.*`    | 2           | 16 kbps |
| `rate3bit.*`    | 3           | 24 kbps |
| `rate4bit.*`    | 4           | 32 kbps |
| `rate5bit.*`    | 5           | 40 kbps |

Per rate: `.payload` is RFC 3551 (LSB-first), `.aal2payload` is AAL2 (MSB-first),
and `.pcm` is the single reference decode shared by both.

## Regeneration (offline, not run in CI)

The vectors are generated with ffmpeg's `g726le` codec (LSB-first, the RFC 3551
RTP order) and its `g726` codec (MSB-first, the AAL2 order), from one shared
source signal. Only the resulting data is committed; no third party code is
vendored.

```sh
# 0.5s 440 Hz sine, 8 kHz mono s16le
ffmpeg -f lavfi -i "sine=frequency=440:sample_rate=8000:duration=0.5" \
       -f s16le -ac 1 in.pcm
for spec in 16k:2 24k:3 32k:4 40k:5; do
  br=${spec%%:*}; cs=${spec##*:}
  ffmpeg -f s16le -ar 8000 -ac 1 -i in.pcm -c:a g726le -b:a $br -f g726le \
         rate${cs}bit.payload
  ffmpeg -f g726le -code_size $cs -ar 8000 -ac 1 -i rate${cs}bit.payload \
         -f s16le rate${cs}bit.pcm
  # Same input, same bit rate, AAL2 (MSB-first) packing. No second .pcm: this
  # must decode to the rate${cs}bit.pcm produced above.
  ffmpeg -f s16le -ar 8000 -ac 1 -i in.pcm -c:a g726 -b:a $br -f g726 \
         rate${cs}bit.aal2payload
done
```

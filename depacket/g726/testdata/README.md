# G.726 conformance vectors

Each `rateNbit.payload` is a raw G.726 RTP payload at N bits per codeword,
packed in RFC 3551 section 4.5.4 order (the first/oldest codeword in the
LEAST significant bits of each octet, little-endian), and the matching
`rateNbit.pcm` is the s16le mono PCM a conformant decoder produces from it.
The decoder in this package must reproduce the `.pcm` file byte-for-byte from
the `.payload` file.

Note on bit order: RFC 3551 section 4.5.4 packs the plain `G726-NN` RTP form
LSB-first, which is ffmpeg's `g726le` (NOT `g726`, which is the opposite,
big-endian, AAL2/I.366.2 order). This package decodes the plain RFC 3551 form.

| file            | bits/sample | rate    |
|-----------------|-------------|---------|
| `rate2bit.*`    | 2           | 16 kbps |
| `rate3bit.*`    | 3           | 24 kbps |
| `rate4bit.*`    | 4           | 32 kbps |
| `rate5bit.*`    | 5           | 40 kbps |

## Regeneration (offline, not run in CI)

The vectors are generated with ffmpeg's `g726le` codec (LSB-first, the RFC 3551
RTP order). Only the resulting data is committed; no third party code is
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
done
```

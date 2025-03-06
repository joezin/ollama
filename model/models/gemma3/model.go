package gemma3

import (
	"bytes"
	"encoding/binary"
	"hash/fnv"
	"image"
	"slices"

	"github.com/ollama/ollama/kvcache"
	"github.com/ollama/ollama/ml"
	"github.com/ollama/ollama/ml/nn"
	"github.com/ollama/ollama/model"
	"github.com/ollama/ollama/model/input"
)

type Model struct {
	model.Base
	model.SentencePieceModel

	*VisionModel `gguf:"v,vision"`
	*TextModel

	*MultiModalProjector `gguf:"mm"`

	ImageProcessor
}

var _ model.MultimodalProcessor = (*Model)(nil)

type MultiModalProjector struct {
	SoftEmbNorm     *nn.RMSNorm `gguf:"mm_soft_emb_norm"`
	InputProjection *nn.Linear  `gguf:"mm_input_projection"`
}

func (p *MultiModalProjector) Forward(ctx ml.Context, visionOutputs ml.Tensor, eps float32) ml.Tensor {
	visionOutputs = p.SoftEmbNorm.Forward(ctx, visionOutputs, eps)

	// TODO: inputProjection must be transposed since they're incompatible with visionOutputs
	visionOutputs = p.InputProjection.Weight.Permute(ctx, 1, 0, 2, 3).Contiguous(ctx).Mulmat(ctx, visionOutputs)
	return visionOutputs
}

func New(c ml.Config) (model.Model, error) {
	m := Model{
		SentencePieceModel: model.NewSentencePieceModel(
			c.String("tokenizer.ggml.pretokenizer", `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`),
			&model.Vocabulary{
				Values: c.Strings("tokenizer.ggml.tokens"),
				Scores: c.Floats("tokenizer.ggml.scores"),
				Types:  c.Uints("tokenizer.ggml.token_type"),
				BOS:    int32(c.Uint("tokenizer.ggml.bos_token_id")),
				AddBOS: c.Bool("tokenizer.ggml.add_bos_token", true),
				EOS:    int32(c.Uint("tokenizer.ggml.eos_token_id")),
				AddEOS: c.Bool("tokenizer.ggml.add_eos_token", false),
			},
		),
		ImageProcessor: newImageProcessor(c),
		VisionModel:    newVisionModel(c),
		TextModel:      newTextModel(c),
	}

	slidingWindowLen := int32(c.Uint("text.attention.sliding_window"))
	m.Cache = kvcache.NewWrapperCache(kvcache.NewSWACache(slidingWindowLen, m.Shift), kvcache.NewCausalCache(m.Shift))

	return &m, nil
}

func (m *Model) EncodeMultimodal(ctx ml.Context, multimodalData []byte) (any, error) {
	image, _, err := image.Decode(bytes.NewReader(multimodalData))
	if err != nil {
		return nil, err
	}

	f32s, err := m.ImageProcessor.ProcessImage(image)
	if err != nil {
		return nil, err
	}

	pixelValues, err := ctx.Input().FromFloatSlice(f32s,
		m.ImageProcessor.imageSize,
		m.ImageProcessor.imageSize,
		m.ImageProcessor.numChannels,
	)
	if err != nil {
		return nil, err
	}

	positionIDs, err := ctx.FromIntSlice([]int32{0}, 1)
	if err != nil {
		return nil, err
	}

	visionOutputs := m.VisionModel.Forward(ctx, pixelValues, positionIDs)

	visionOutputs = visionOutputs.Permute(ctx, 1, 0, 2, 3).Contiguous(ctx)
	patchesPerImage := m.ImageProcessor.imageSize / m.ImageProcessor.patchSize
	kernelSize := patchesPerImage * patchesPerImage / 256
	visionOutputs = visionOutputs.AvgPool1D(ctx, kernelSize, kernelSize, 0)

	visionOutputs = visionOutputs.Permute(ctx, 1, 0, 2, 3).Contiguous(ctx)
	visionOutputs = m.MultiModalProjector.Forward(ctx, visionOutputs, m.VisionModel.eps)
	return visionOutputs, nil
}

func (m *Model) PostTokenize(ctx ml.Context, inputs []input.Input) ([]input.Input, error) {
	var images []input.Input
	fnvHash := fnv.New64a()

	for i := range inputs {
		if inputs[i].Multimodal == nil {
			if len(images) > 0 {
				inputs[i].Multimodal = images[0].Multimodal
				inputs[i].MultimodalHash = images[0].MultimodalHash
				for j := 1; j < len(images); j++ {
					inputs[i].Multimodal = inputs[i].Multimodal.(ml.Tensor).Concat(ctx, images[j].Multimodal.(ml.Tensor), 3)
					fnvHash.Reset()
					binary.Write(fnvHash, binary.NativeEndian, inputs[i].MultimodalHash)
					binary.Write(fnvHash, binary.NativeEndian, inputs[j].MultimodalHash)
					inputs[i].MultimodalHash = fnvHash.Sum64()
				}
				images = nil
			}
		} else {
			images = append(images, inputs[i])
			inputs[i].Token = -1
		}
	}

	inputs = slices.DeleteFunc(inputs, func(input input.Input) bool { return input.Token == -1 })

	return inputs, nil
}

func (m *Model) Forward(ctx ml.Context, opts input.Options) (ml.Tensor, error) {
	var embeddings ml.Tensor
	if opts.Multimodal != nil {
		embeddings = opts.Multimodal[0].Multimodal.(ml.Tensor)
	}

	inputs, err := ctx.Input().FromIntSlice(opts.Inputs, len(opts.Inputs))
	if err != nil {
		return nil, err
	}

	positions, err := ctx.Input().FromIntSlice(opts.Positions, len(opts.Positions))
	if err != nil {
		return nil, err
	}

	outputs, err := ctx.Output().FromIntSlice(opts.Outputs, len(opts.Outputs))
	if err != nil {
		return nil, err
	}

	return m.TextModel.Forward(ctx, inputs, positions, embeddings, outputs, m.Cache), nil
}

func init() {
	model.Register("gemma3", New)
}

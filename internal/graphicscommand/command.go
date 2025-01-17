// Copyright 2016 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package graphicscommand

import (
	"fmt"
	"math"
	"strings"

	"github.com/hajimehoshi/ebiten/v2/internal/debug"
	"github.com/hajimehoshi/ebiten/v2/internal/graphics"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
	"github.com/hajimehoshi/ebiten/v2/internal/shaderir"
)

// command represents a drawing command.
//
// A command for drawing that is created when Image functions are called like DrawTriangles,
// or Fill.
// A command is not immediately executed after created. Instaed, it is queued after created,
// and executed only when necessary.
type command interface {
	fmt.Stringer

	Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error
}

type drawTrianglesCommandPool struct {
	pool []*drawTrianglesCommand
}

func (p *drawTrianglesCommandPool) get() *drawTrianglesCommand {
	if len(p.pool) == 0 {
		return &drawTrianglesCommand{}
	}
	v := p.pool[len(p.pool)-1]
	p.pool = p.pool[:len(p.pool)-1]
	return v
}

func (p *drawTrianglesCommandPool) put(v *drawTrianglesCommand) {
	if len(p.pool) >= 1024 {
		return
	}
	p.pool = append(p.pool, v)
}

// commandQueue is a command queue for drawing commands.
type commandQueue struct {
	// commands is a queue of drawing commands.
	commands []command

	// vertices represents a vertices data in OpenGL's array buffer.
	vertices []float32
	indices  []uint16

	tmpNumVertexFloats int
	tmpNumIndices      int

	drawTrianglesCommandPool drawTrianglesCommandPool

	float32sBuffer []float32
}

// theCommandQueue is the command queue for the current process.
var theCommandQueue = &commandQueue{}

func (q *commandQueue) appendIndices(indices []uint16, offset uint16) {
	n := len(q.indices)
	q.indices = append(q.indices, indices...)
	for i := range indices {
		q.indices[n+i] += offset
	}
}

// mustUseDifferentVertexBuffer reports whether a different vertex buffer must be used.
func mustUseDifferentVertexBuffer(nextNumVertexFloats, nextNumIndices int) bool {
	return nextNumVertexFloats > graphics.IndicesCount*graphics.VertexFloatCount || nextNumIndices > graphics.IndicesCount
}

// EnqueueDrawTrianglesCommand enqueues a drawing-image command.
func (q *commandQueue) EnqueueDrawTrianglesCommand(dst *Image, srcs [graphics.ShaderImageCount]*Image, offsets [graphics.ShaderImageCount - 1][2]float32, vertices []float32, indices []uint16, blend graphicsdriver.Blend, dstRegion, srcRegion graphicsdriver.Region, shader *Shader, uniforms [][]float32, evenOdd bool) {
	if len(indices) > graphics.IndicesCount {
		panic(fmt.Sprintf("graphicscommand: len(indices) must be <= graphics.IndicesCount but not at EnqueueDrawTrianglesCommand: len(indices): %d, graphics.IndicesCount: %d", len(indices), graphics.IndicesCount))
	}

	split := false
	if mustUseDifferentVertexBuffer(q.tmpNumVertexFloats+len(vertices), q.tmpNumIndices+len(indices)) {
		q.tmpNumVertexFloats = 0
		q.tmpNumIndices = 0
		split = true
	}

	// Assume that all the image sizes are same.
	// Assume that the images are packed from the front in the slice srcs.
	q.vertices = append(q.vertices, vertices...)
	q.appendIndices(indices, uint16(q.tmpNumVertexFloats/graphics.VertexFloatCount))
	q.tmpNumVertexFloats += len(vertices)
	q.tmpNumIndices += len(indices)

	uniforms = q.prependPreservedUniforms(uniforms, dst, srcs, offsets, dstRegion, srcRegion)

	// Remove unused uniform variables so that more commands can be merged.
	shader.ir.FilterUniformVariables(uniforms)

	// TODO: If dst is the screen, reorder the command to be the last.
	if !split && 0 < len(q.commands) {
		if last, ok := q.commands[len(q.commands)-1].(*drawTrianglesCommand); ok {
			if last.CanMergeWithDrawTrianglesCommand(dst, srcs, vertices, blend, dstRegion, shader, uniforms, evenOdd) {
				last.setVertices(q.lastVertices(len(vertices) + last.numVertices()))
				last.addNumIndices(len(indices))
				return
			}
		}
	}

	c := q.drawTrianglesCommandPool.get()
	c.dst = dst
	c.srcs = srcs
	c.vertices = q.lastVertices(len(vertices))
	c.nindices = len(indices)
	c.blend = blend
	c.dstRegion = dstRegion
	c.shader = shader
	c.uniforms = uniforms
	c.evenOdd = evenOdd
	q.commands = append(q.commands, c)
}

func (q *commandQueue) lastVertices(n int) []float32 {
	return q.vertices[len(q.vertices)-n : len(q.vertices)]
}

// Enqueue enqueues a drawing command other than a draw-triangles command.
//
// For a draw-triangles command, use EnqueueDrawTrianglesCommand.
func (q *commandQueue) Enqueue(command command) {
	// TODO: If dst is the screen, reorder the command to be the last.
	q.commands = append(q.commands, command)
}

// Flush flushes the command queue.
func (q *commandQueue) Flush(graphicsDriver graphicsdriver.Graphics, endFrame bool) (err error) {
	runOnRenderingThread(func() {
		err = q.flush(graphicsDriver, endFrame)
	})
	if endFrame {
		q.float32sBuffer = q.float32sBuffer[:0]
	}
	return
}

// flush must be called the main thread.
func (q *commandQueue) flush(graphicsDriver graphicsdriver.Graphics, endFrame bool) (err error) {
	// If endFrame is true, Begin/End should be called to ensure the framebuffer is swapped.
	if len(q.commands) == 0 && !endFrame {
		return nil
	}

	es := q.indices
	vs := q.vertices
	debug.Logf("Graphics commands:\n")

	if err := graphicsDriver.Begin(); err != nil {
		return err
	}

	defer func() {
		// Call End even if an error causes, or the graphics driver's state might be stale (#2388).
		if err1 := graphicsDriver.End(endFrame); err1 != nil && err == nil {
			err = err1
		}

		// Release the commands explicitly (#1803).
		// Apparently, the part of a slice between len and cap-1 still holds references.
		// Then, resetting the length by [:0] doesn't release the references.
		for i, c := range q.commands {
			if c, ok := c.(*drawTrianglesCommand); ok {
				q.drawTrianglesCommandPool.put(c)
			}
			q.commands[i] = nil
		}
		q.commands = q.commands[:0]
		q.vertices = q.vertices[:0]
		q.indices = q.indices[:0]
		q.tmpNumVertexFloats = 0
		q.tmpNumIndices = 0
	}()

	cs := q.commands
	for len(cs) > 0 {
		nv := 0
		ne := 0
		nc := 0
		for _, c := range cs {
			if dtc, ok := c.(*drawTrianglesCommand); ok {
				if dtc.numIndices() > graphics.IndicesCount {
					panic(fmt.Sprintf("graphicscommand: dtc.NumIndices() must be <= graphics.IndicesCount but not at Flush: dtc.NumIndices(): %d, graphics.IndicesCount: %d", dtc.numIndices(), graphics.IndicesCount))
				}
				if nc > 0 && mustUseDifferentVertexBuffer(nv+dtc.numVertices(), ne+dtc.numIndices()) {
					break
				}
				nv += dtc.numVertices()
				ne += dtc.numIndices()
			}
			nc++
		}
		if 0 < ne {
			if err := graphicsDriver.SetVertices(vs[:nv], es[:ne]); err != nil {
				return err
			}
			es = es[ne:]
			vs = vs[nv:]
		}
		indexOffset := 0
		for _, c := range cs[:nc] {
			if err := c.Exec(graphicsDriver, indexOffset); err != nil {
				return err
			}
			debug.Logf("  %s\n", c)
			// TODO: indexOffset should be reset if the command type is different
			// from the previous one. This fix is needed when another drawing command is
			// introduced than drawTrianglesCommand.
			if dtc, ok := c.(*drawTrianglesCommand); ok {
				indexOffset += dtc.numIndices()
			}
		}
		cs = cs[nc:]
	}

	return nil
}

// FlushCommands flushes the command queue and present the screen if needed.
// If endFrame is true, the current screen might be used to present.
func FlushCommands(graphicsDriver graphicsdriver.Graphics, endFrame bool) error {
	flushImageBuffers()
	return theCommandQueue.Flush(graphicsDriver, endFrame)
}

// drawTrianglesCommand represents a drawing command to draw an image on another image.
type drawTrianglesCommand struct {
	dst       *Image
	srcs      [graphics.ShaderImageCount]*Image
	vertices  []float32
	nindices  int
	blend     graphicsdriver.Blend
	dstRegion graphicsdriver.Region
	shader    *Shader
	uniforms  [][]float32
	evenOdd   bool
}

func (c *drawTrianglesCommand) String() string {
	// TODO: Improve readability
	blend := fmt.Sprintf("{src-color: %d, src-alpha:  %d, dst-color: %d, dst-alpha: %d, op-color: %d, op-alpha: %d}",
		c.blend.BlendFactorSourceRGB,
		c.blend.BlendFactorSourceAlpha,
		c.blend.BlendFactorDestinationRGB,
		c.blend.BlendFactorDestinationAlpha,
		c.blend.BlendOperationRGB,
		c.blend.BlendOperationAlpha)

	dst := fmt.Sprintf("%d", c.dst.id)
	if c.dst.screen {
		dst += " (screen)"
	}

	var srcstrs [graphics.ShaderImageCount]string
	for i, src := range c.srcs {
		if src == nil {
			srcstrs[i] = "(nil)"
			continue
		}
		srcstrs[i] = fmt.Sprintf("%d", src.id)
		if src.screen {
			srcstrs[i] += " (screen)"
		}
	}

	r := fmt.Sprintf("(x:%d, y:%d, width:%d, height:%d)",
		int(c.dstRegion.X), int(c.dstRegion.Y), int(c.dstRegion.Width), int(c.dstRegion.Height))
	return fmt.Sprintf("draw-triangles: dst: %s <- src: [%s], dst region: %s, num of indices: %d, blend: %s, even-odd: %t", dst, strings.Join(srcstrs[:], ", "), r, c.nindices, blend, c.evenOdd)
}

// Exec executes the drawTrianglesCommand.
func (c *drawTrianglesCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	// TODO: Is it ok not to bind any framebuffer here?
	if c.nindices == 0 {
		return nil
	}

	var imgs [graphics.ShaderImageCount]graphicsdriver.ImageID
	for i, src := range c.srcs {
		if src == nil {
			imgs[i] = graphicsdriver.InvalidImageID
			continue
		}
		imgs[i] = src.image.ID()
	}

	return graphicsDriver.DrawTriangles(c.dst.image.ID(), imgs, c.shader.shader.ID(), c.nindices, indexOffset, c.blend, c.dstRegion, c.uniforms, c.evenOdd)
}

func (c *drawTrianglesCommand) numVertices() int {
	return len(c.vertices)
}

func (c *drawTrianglesCommand) numIndices() int {
	return c.nindices
}

func (c *drawTrianglesCommand) setVertices(vertices []float32) {
	c.vertices = vertices
}

func (c *drawTrianglesCommand) addNumIndices(n int) {
	c.nindices += n
}

// CanMergeWithDrawTrianglesCommand returns a boolean value indicating whether the other drawTrianglesCommand can be merged
// with the drawTrianglesCommand c.
func (c *drawTrianglesCommand) CanMergeWithDrawTrianglesCommand(dst *Image, srcs [graphics.ShaderImageCount]*Image, vertices []float32, blend graphicsdriver.Blend, dstRegion graphicsdriver.Region, shader *Shader, uniforms [][]float32, evenOdd bool) bool {
	if c.shader != shader {
		return false
	}
	if len(c.uniforms) != len(uniforms) {
		return false
	}
	for i := range c.uniforms {
		if len(c.uniforms[i]) != len(uniforms[i]) {
			return false
		}
		for j := range c.uniforms[i] {
			if c.uniforms[i][j] != uniforms[i][j] {
				return false
			}
		}
	}
	if c.dst != dst {
		return false
	}
	if c.srcs != srcs {
		return false
	}
	if c.blend != blend {
		return false
	}
	if c.dstRegion != dstRegion {
		return false
	}
	if c.evenOdd || evenOdd {
		if c.evenOdd && evenOdd {
			return !mightOverlapDstRegions(c.vertices, vertices)
		}
		return false
	}
	return true
}

var (
	posInf32 = float32(math.Inf(1))
	negInf32 = float32(math.Inf(-1))
)

func dstRegionFromVertices(vertices []float32) (minX, minY, maxX, maxY float32) {
	minX = posInf32
	minY = posInf32
	maxX = negInf32
	maxY = negInf32

	for i := 0; i < len(vertices)/graphics.VertexFloatCount; i++ {
		x := vertices[graphics.VertexFloatCount*i]
		y := vertices[graphics.VertexFloatCount*i+1]
		if x < minX {
			minX = x
		}
		if y < minY {
			minY = y
		}
		if maxX < x {
			maxX = x
		}
		if maxY < y {
			maxY = y
		}
	}
	return
}

func mightOverlapDstRegions(vertices1, vertices2 []float32) bool {
	minX1, minY1, maxX1, maxY1 := dstRegionFromVertices(vertices1)
	minX2, minY2, maxX2, maxY2 := dstRegionFromVertices(vertices2)
	const mergin = 1
	return minX1 < maxX2+mergin && minX2 < maxX1+mergin && minY1 < maxY2+mergin && minY2 < maxY1+mergin
}

// writePixelsCommand represents a command to replace pixels of an image.
type writePixelsCommand struct {
	dst  *Image
	args []*graphicsdriver.WritePixelsArgs
}

func (c *writePixelsCommand) String() string {
	return fmt.Sprintf("write-pixels: dst: %d, len(args): %d", c.dst.id, len(c.args))
}

// Exec executes the writePixelsCommand.
func (c *writePixelsCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	if len(c.args) == 0 {
		return nil
	}
	if err := c.dst.image.WritePixels(c.args); err != nil {
		return err
	}
	return nil
}

type readPixelsCommand struct {
	result []byte
	img    *Image
	x      int
	y      int
	width  int
	height int
}

// Exec executes a readPixelsCommand.
func (c *readPixelsCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	if err := c.img.image.ReadPixels(c.result, c.x, c.y, c.width, c.height); err != nil {
		return err
	}
	return nil
}

func (c *readPixelsCommand) String() string {
	return fmt.Sprintf("read-pixels: image: %d", c.img.id)
}

// disposeImageCommand represents a command to dispose an image.
type disposeImageCommand struct {
	target *Image
}

func (c *disposeImageCommand) String() string {
	return fmt.Sprintf("dispose-image: target: %d", c.target.id)
}

// Exec executes the disposeImageCommand.
func (c *disposeImageCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	c.target.image.Dispose()
	return nil
}

// disposeShaderCommand represents a command to dispose a shader.
type disposeShaderCommand struct {
	target *Shader
}

func (c *disposeShaderCommand) String() string {
	return fmt.Sprintf("dispose-shader: target")
}

// Exec executes the disposeShaderCommand.
func (c *disposeShaderCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	c.target.shader.Dispose()
	return nil
}

// newImageCommand represents a command to create an empty image with given width and height.
type newImageCommand struct {
	result *Image
	width  int
	height int
	screen bool
}

func (c *newImageCommand) String() string {
	return fmt.Sprintf("new-image: result: %d, width: %d, height: %d, screen: %t", c.result.id, c.width, c.height, c.screen)
}

// Exec executes a newImageCommand.
func (c *newImageCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	var err error
	if c.screen {
		c.result.image, err = graphicsDriver.NewScreenFramebufferImage(c.width, c.height)
	} else {
		c.result.image, err = graphicsDriver.NewImage(c.width, c.height)
	}
	return err
}

// newShaderCommand is a command to create a shader.
type newShaderCommand struct {
	result *Shader
	ir     *shaderir.Program
}

func (c *newShaderCommand) String() string {
	return fmt.Sprintf("new-shader")
}

// Exec executes a newShaderCommand.
func (c *newShaderCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	s, err := graphicsDriver.NewShader(c.ir)
	if err != nil {
		return err
	}
	c.result.shader = s
	return nil
}

type isInvalidatedCommand struct {
	result bool
	image  *Image
}

func (c *isInvalidatedCommand) String() string {
	return fmt.Sprintf("is-invalidated: image: %d", c.image.id)
}

func (c *isInvalidatedCommand) Exec(graphicsDriver graphicsdriver.Graphics, indexOffset int) error {
	c.result = c.image.image.IsInvalidated()
	return nil
}

// InitializeGraphicsDriverState initialize the current graphics driver state.
func InitializeGraphicsDriverState(graphicsDriver graphicsdriver.Graphics) (err error) {
	runOnRenderingThread(func() {
		err = graphicsDriver.Initialize()
	})
	return
}

// ResetGraphicsDriverState resets the current graphics driver state.
// If the graphics driver doesn't have an API to reset, ResetGraphicsDriverState does nothing.
func ResetGraphicsDriverState(graphicsDriver graphicsdriver.Graphics) (err error) {
	if r, ok := graphicsDriver.(interface{ Reset() error }); ok {
		runOnRenderingThread(func() {
			err = r.Reset()
		})
	}
	return nil
}

// MaxImageSize returns the maximum size of an image.
func MaxImageSize(graphicsDriver graphicsdriver.Graphics) int {
	var size int
	runOnRenderingThread(func() {
		size = graphicsDriver.MaxImageSize()
	})
	return size
}

func max(a, b int) int {
	if a < b {
		return b
	}
	return a
}

func roundUpPower2(x int) int {
	p2 := 1
	for p2 < x {
		p2 *= 2
	}
	return p2
}

func (q *commandQueue) allocFloat32s(n int) []float32 {
	buf := q.float32sBuffer
	if len(buf)+n > cap(buf) {
		buf = make([]float32, 0, max(roundUpPower2(len(buf)+n), 16))
	}
	s := buf[len(buf) : len(buf)+n]
	q.float32sBuffer = buf[:len(buf)+n]
	return s
}

func (q *commandQueue) prependPreservedUniforms(uniforms [][]float32, dst *Image, srcs [graphics.ShaderImageCount]*Image, offsets [graphics.ShaderImageCount - 1][2]float32, dstRegion, srcRegion graphicsdriver.Region) [][]float32 {
	uniforms = append(uniforms, make([][]float32, graphics.PreservedUniformVariablesCount)...)
	copy(uniforms[graphics.PreservedUniformVariablesCount:], uniforms)

	// Set the destination texture size.
	dw, dh := dst.InternalSize()
	udstsize := q.allocFloat32s(2)
	udstsize[0] = float32(dw)
	udstsize[1] = float32(dh)
	uniforms[graphics.TextureDestinationSizeUniformVariableIndex] = udstsize

	// Set the source texture sizes.
	usizes := q.allocFloat32s(2 * len(srcs))
	for i, src := range srcs {
		if src != nil {
			w, h := src.InternalSize()
			usizes[2*i] = float32(w)
			usizes[2*i+1] = float32(h)
		}
	}
	uniforms[graphics.TextureSourceSizesUniformVariableIndex] = usizes

	// Set the destination region.
	udstrorig := q.allocFloat32s(2)
	udstrorig[0] = float32(dstRegion.X) / float32(dw)
	udstrorig[1] = float32(dstRegion.Y) / float32(dh)
	uniforms[graphics.TextureDestinationRegionOriginUniformVariableIndex] = udstrorig

	udstrsize := q.allocFloat32s(2)
	udstrsize[0] = float32(dstRegion.Width) / float32(dw)
	udstrsize[1] = float32(dstRegion.Height) / float32(dh)
	uniforms[graphics.TextureDestinationRegionSizeUniformVariableIndex] = udstrsize

	if srcs[0] != nil {
		w, h := srcs[0].InternalSize()
		srcRegion.X /= float32(w)
		srcRegion.Y /= float32(h)
		srcRegion.Width /= float32(w)
		srcRegion.Height /= float32(h)
		for i := range offsets {
			offsets[i][0] /= float32(w)
			offsets[i][1] /= float32(h)
		}
	}

	// Set the source offsets.
	uoffsets := q.allocFloat32s(2 * len(offsets))
	for i, offset := range offsets {
		uoffsets[2*i] = offset[0]
		uoffsets[2*i+1] = offset[1]
	}
	uniforms[graphics.TextureSourceOffsetsUniformVariableIndex] = uoffsets

	// Set the source region of texture0.
	usrcrorig := q.allocFloat32s(2)
	usrcrorig[0] = float32(srcRegion.X)
	usrcrorig[1] = float32(srcRegion.Y)
	uniforms[graphics.TextureSourceRegionOriginUniformVariableIndex] = usrcrorig

	usrcrsize := q.allocFloat32s(2)
	usrcrsize[0] = float32(srcRegion.Width)
	usrcrsize[1] = float32(srcRegion.Height)
	uniforms[graphics.TextureSourceRegionSizeUniformVariableIndex] = usrcrsize

	umatrix := q.allocFloat32s(16)
	umatrix[0] = 2 / float32(dw)
	umatrix[1] = 0
	umatrix[2] = 0
	umatrix[3] = 0
	umatrix[4] = 0
	umatrix[5] = 2 / float32(dh)
	umatrix[6] = 0
	umatrix[7] = 0
	umatrix[8] = 0
	umatrix[9] = 0
	umatrix[10] = 1
	umatrix[11] = 0
	umatrix[12] = -1
	umatrix[13] = -1
	umatrix[14] = 0
	umatrix[15] = 1
	uniforms[graphics.ProjectionMatrixUniformVariableIndex] = umatrix

	return uniforms
}
